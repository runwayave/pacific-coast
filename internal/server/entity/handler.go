package entity

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/codegen/query"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// dispatch routes an RPC to the correct handler based on the op string.
func (s *Server) dispatch(ctx context.Context, meta *entityMeta, op string, dec func(any) error) (any, error) {
	switch op {
	case "Get":
		return s.handleGet(ctx, meta, dec)
	case "Create":
		return s.handleCreate(ctx, meta, dec)
	case "Update":
		return s.handleUpdate(ctx, meta, dec)
	case "Delete":
		return s.handleDelete(ctx, meta, dec)
	case "BatchGet":
		return s.handleBatchGet(ctx, meta, dec)
	case "Query":
		return s.handleQuery(ctx, meta, dec)
	default:
		return nil, status.Errorf(codes.Unimplemented, "unknown operation %s%s", op, meta.entity.Name)
	}
}

// makeHandler returns a grpc.MethodDesc.Handler for one RPC method.
// It captures the entity ID (immutable string) rather than a pointer
// to entityMeta, and loads the current metadata from the snapshot at
// each request. This allows the snapshot to be swapped for hot-reload.
func makeHandler(s *Server, entityID string, op string, ns string, name string) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	fullMethod := fmt.Sprintf("/atlantis.%s.v1.%sService/%s%s", ns, name, op, name)

	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		snap := s.snapshot.Load()
		meta, ok := snap.entities[entityID]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "entity %s not found in current schema", entityID)
		}
		if interceptor == nil {
			return s.dispatch(ctx, meta, op, dec)
		}
		info := &grpc.UnaryServerInfo{
			Server:     srv,
			FullMethod: fullMethod,
		}
		handler := func(ctx context.Context, _ any) (any, error) {
			return s.dispatch(ctx, meta, op, dec)
		}
		return interceptor(ctx, nil, info, handler)
	}
}

// handleGet delegates to handleQuery with a PK equality filter. This
// matches the generated code pattern where GetX is a thin wrapper
// around QueryX with a PK-eq filter.
func (s *Server) handleGet(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.getRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract PK values from the request.
	pkArgs := make([]any, 0, len(meta.pkCols))
	for i, cm := range meta.pkCols {
		fd := meta.getRequestDesc.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
		if fd == nil {
			return nil, fmt.Errorf("Get%s: missing PK field %s in request", meta.entity.Name, cm.sqlName)
		}
		pkArgs = append(pkArgs, goValueFromProto(req, fd, cm))
	}

	row := s.pool.QueryRow(ctx, meta.sqlGet, pkArgs...)
	entity, err := scanRow(meta, row)
	if err != nil {
		if runtime.IsNoRows(err) {
			return nil, runtime.ErrNotFound
		}
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.getResponseDesc)
	entityFD := meta.getResponseDesc.Fields().ByName("entity")
	if entityFD != nil {
		resp.Set(entityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleCreate(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.createRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract the entity sub-message from the "entity" field.
	entityFD := meta.createRequestDesc.Fields().ByName("entity")
	if entityFD == nil {
		return nil, fmt.Errorf("Create%s: request missing entity field", meta.entity.Name)
	}
	if !req.Has(entityFD) {
		return nil, fmt.Errorf("Create%s: entity is required", meta.entity.Name)
	}
	entityMsg, ok := req.Get(entityFD).Message().Interface().(*dynamicpb.Message)
	if !ok {
		return nil, fmt.Errorf("Create%s: entity is not a dynamic message", meta.entity.Name)
	}

	args := bindForInsert(meta, entityMsg)

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// INSERT RETURNING pk.
	pkScanTargets := makePKScanTargets(meta)
	row := tx.QueryRow(ctx, meta.sqlInsert, args...)
	if err := row.Scan(pkScanTargets...); err != nil {
		return nil, err
	}

	// Build cache ID from returned PK.
	pkValues := readPKScanTargets(meta, pkScanTargets)
	cacheID := runtime.CompositeID(pkValues...)

	// Outbox invalidation.
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Load fresh row.
	freshRow := s.pool.QueryRow(ctx, meta.sqlGet, pkValues...)
	entity, err := scanRow(meta, freshRow)
	if err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.createResponseDesc)
	respEntityFD := meta.createResponseDesc.Fields().ByName("entity")
	if respEntityFD != nil {
		resp.Set(respEntityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleUpdate(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	if meta.sqlUpdate == "" {
		return nil, status.Errorf(codes.Unimplemented, "Update%s: entity has no updatable columns", meta.entity.Name)
	}

	req := dynamicpb.NewMessage(meta.updateRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	entityFD := meta.updateRequestDesc.Fields().ByName("entity")
	if entityFD == nil {
		return nil, fmt.Errorf("Update%s: request missing entity field", meta.entity.Name)
	}
	if !req.Has(entityFD) {
		return nil, fmt.Errorf("Update%s: entity is required", meta.entity.Name)
	}
	entityMsg, ok := req.Get(entityFD).Message().Interface().(*dynamicpb.Message)
	if !ok {
		return nil, fmt.Errorf("Update%s: entity is not a dynamic message", meta.entity.Name)
	}

	args := bindForUpdate(meta, entityMsg)
	pkValues := extractPKValues(meta, entityMsg)

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, meta.sqlUpdate, args...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, runtime.ErrNotFound
	}

	cacheID := runtime.CompositeID(pkValues...)
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Load fresh row.
	freshRow := s.pool.QueryRow(ctx, meta.sqlGet, pkValues...)
	entity, err := scanRow(meta, freshRow)
	if err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.updateResponseDesc)
	respEntityFD := meta.updateResponseDesc.Fields().ByName("entity")
	if respEntityFD != nil {
		resp.Set(respEntityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleDelete(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.deleteRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	pkArgs := make([]any, 0, len(meta.pkCols))
	for i, cm := range meta.pkCols {
		fd := meta.deleteRequestDesc.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
		if fd == nil {
			return nil, fmt.Errorf("Delete%s: missing PK field %s", meta.entity.Name, cm.sqlName)
		}
		pkArgs = append(pkArgs, goValueFromProto(req, fd, cm))
	}

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, meta.sqlDelete, pkArgs...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, runtime.ErrNotFound
	}

	cacheID := runtime.CompositeID(pkArgs...)
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.deleteResponseDesc)
	return resp, nil
}

// handleBatchGet uses ANY($1) for single-PK entities; composite PKs
// fall back to individual gets.
func (s *Server) handleBatchGet(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.batchGetRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.batchGetResponseDesc)
	entitiesFD := meta.batchGetResponseDesc.Fields().ByName("entities")
	if entitiesFD == nil {
		return resp, nil
	}

	composite := len(meta.pkCols) > 1

	if composite {
		// Composite PK: individual gets.
		idsFD := meta.batchGetRequestDesc.Fields().ByName("ids")
		if idsFD == nil {
			return resp, nil
		}
		list := req.Get(idsFD).List()
		entities := resp.Mutable(entitiesFD).List()
		for i := 0; i < list.Len(); i++ {
			pkMsg := list.Get(i).Message()
			pkArgs := make([]any, len(meta.pkCols))
			for j := range meta.pkCols {
				pkFD := pkMsg.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(j + 1))
				if pkFD != nil {
					pkArgs[j] = goValueFromProtoReflect(pkMsg, pkFD, meta.pkCols[j])
				}
			}
			row := s.pool.QueryRow(ctx, meta.sqlGet, pkArgs...)
			entity, err := scanRow(meta, row)
			if err != nil {
				if runtime.IsNoRows(err) {
					continue
				}
				return nil, err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
	} else {
		// Single PK: use ANY($1).
		pkField := meta.batchGetRequestDesc.Fields().Get(0) // first field is the repeated PK
		if pkField == nil {
			return resp, nil
		}
		list := req.Get(pkField).List()
		if list.Len() == 0 {
			return resp, nil
		}
		if list.Len() > 200 {
			return nil, status.Errorf(codes.InvalidArgument,
				"BatchGet%s: at most 200 ids per call (got %d)", meta.entity.Name, list.Len())
		}

		// Build the array arg.
		pkSlice := buildPKArray(meta.pkCols[0], list)

		rows, err := s.pool.Query(ctx, meta.sqlBatchGet, pkSlice)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		entities := resp.Mutable(entitiesFD).List()
		for rows.Next() {
			entity, err := scanRow(meta, rows)
			if err != nil {
				return nil, err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// handleQuery applies TranslateFilter + keyset pagination.
func (s *Server) handleQuery(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.queryRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract limit.
	limitFD := meta.queryRequestDesc.Fields().ByName("limit")
	limit := int32(100)
	if limitFD != nil && req.Has(limitFD) {
		limit = int32(req.Get(limitFD).Int())
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Extract page_token.
	pageTokenFD := meta.queryRequestDesc.Fields().ByName("page_token")
	pageToken := ""
	if pageTokenFD != nil && req.Has(pageTokenFD) {
		pageToken = req.Get(pageTokenFD).String()
	}

	// Extract filter.
	filterFD := meta.queryRequestDesc.Fields().ByName("filter")
	var filterMsg protoreflect.Message
	if filterFD != nil && req.Has(filterFD) {
		filterMsg = req.Get(filterFD).Message()
	}

	// Build keyset columns: default to PK ascending (no custom order
	// support in the dynamic path yet — the generated code has typed
	// order enums; the dynamic server defaults to PK order).
	keysetCols := buildDefaultKeysetCols(meta)

	// Decode cursor.
	cursorVals, err := runtime.DecodePageToken(pageToken, meta.entityID)
	if err != nil {
		return nil, err
	}

	// Translate filter.
	extras := make([]string, 0, 2)

	// Soft delete filter.
	if meta.entity.SoftDeleteField != "" {
		extras = append(extras, schema.QuoteIdent(meta.entity.SoftDeleteField)+" IS NULL")
	}

	where, args, _, err := query.TranslateFilter(meta.filterSpec, filterMsg, 1, extras...)
	if err != nil {
		return nil, err
	}

	// Keyset predicate.
	if len(cursorVals) > 0 {
		cursorSQL, cursorArgs, kerr := runtime.KeysetPredicate(keysetCols, cursorVals, len(args)+1)
		if kerr != nil {
			return nil, kerr
		}
		if where == "" {
			where = cursorSQL
		} else {
			where = where + " AND " + cursorSQL
		}
		args = append(args, cursorArgs...)
	}

	// Assemble SQL.
	var b strings.Builder
	b.WriteString(meta.sqlQueryPrefix)
	if where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	b.WriteString(runtime.OrderByClauseFromKeyset(keysetCols))
	args = append(args, limit+1)
	fmt.Fprintf(&b, " LIMIT $%d", len(args))
	sqlText := b.String()

	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resp := dynamicpb.NewMessage(meta.queryResponseDesc)
	entitiesFD := meta.queryResponseDesc.Fields().ByName("entities")
	if entitiesFD == nil {
		return resp, nil
	}
	entities := resp.Mutable(entitiesFD).List()

	for rows.Next() {
		entity, err := scanRow(meta, rows)
		if err != nil {
			return nil, err
		}
		entities.Append(protoreflect.ValueOfMessage(entity))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Pagination: if we got more rows than the limit, trim and set next_page_token.
	if int32(entities.Len()) > limit {
		// Extract cursor from the boundary row (the limit-th entity, 0-indexed).
		boundaryEntity := entities.Get(int(limit) - 1).Message().Interface().(*dynamicpb.Message)

		// Trim to limit.
		entities.Truncate(int(limit))

		cursorOut := extractCursorValues(meta, boundaryEntity, keysetCols)
		nextToken, _ := runtime.EncodePageToken(meta.entityID, cursorOut)
		nextTokenFD := meta.queryResponseDesc.Fields().ByName("next_page_token")
		if nextTokenFD != nil {
			resp.Set(nextTokenFD, protoreflect.ValueOfString(nextToken))
		}
	}

	return resp, nil
}

func buildDefaultKeysetCols(meta *entityMeta) []runtime.KeysetColumn {
	cols := make([]runtime.KeysetColumn, 0, len(meta.pkCols))
	for _, pk := range meta.pkCols {
		cols = append(cols, runtime.KeysetColumn{
			QuotedIdent: schema.QuoteIdent(pk.sqlName),
			Desc:        false,
		})
	}
	return cols
}

// extractCursorValues extracts cursor values for keyset pagination.
func extractCursorValues(meta *entityMeta, entity *dynamicpb.Message, keysetCols []runtime.KeysetColumn) []any {
	out := make([]any, 0, len(keysetCols))
	for _, kc := range keysetCols {
		// Strip quotes from the ident to match column names.
		colName := strings.Trim(kc.QuotedIdent, `"`)
		for _, cm := range meta.columns {
			if cm.sqlName == colName {
				fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
				out = append(out, protoValueForCursor(entity, fd, cm))
				break
			}
		}
	}
	return out
}

// goValueFromProto extracts a Go value for SQL arguments.
func goValueFromProto(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	return goValueFromProtoReflect(msg, fd, cm)
}

func goValueFromProtoReflect(msg protoreflect.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	t := cm.field.Type
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		return msg.Get(fd).String()
	case "bigint":
		return msg.Get(fd).Int()
	case "int", "smallint":
		return int32(msg.Get(fd).Int())
	case "boolean":
		return msg.Get(fd).Bool()
	case "timestamptz", "date":
		sub := msg.Get(fd).Message()
		secFD := sub.Descriptor().Fields().ByName("seconds")
		if secFD == nil {
			return nil
		}
		sec := sub.Get(secFD).Int()
		nanoFD := sub.Descriptor().Fields().ByName("nanos")
		var nanos int32
		if nanoFD != nil {
			nanos = int32(sub.Get(nanoFD).Int())
		}
		return fmt.Sprintf("%v", sec+int64(nanos))
	case "bytea", "jsonb":
		return msg.Get(fd).Bytes()
	}
	return msg.Get(fd).Interface()
}

// buildPKArray builds a typed Go slice for the ANY($1) SQL pattern.
func buildPKArray(pk columnMeta, list protoreflect.List) any {
	switch pk.field.Type.Name {
	case "text", "varchar", "citext", "uuid":
		out := make([]string, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).String()
		}
		return out
	case "bigint":
		out := make([]int64, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Int()
		}
		return out
	case "int", "smallint":
		out := make([]int32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = int32(list.Get(i).Int())
		}
		return out
	}
	// Fallback: []string.
	out := make([]string, list.Len())
	for i := 0; i < list.Len(); i++ {
		out[i] = fmt.Sprintf("%v", list.Get(i).Interface())
	}
	return out
}

// makePKScanTargets allocates scan targets for INSERT ... RETURNING.
func makePKScanTargets(meta *entityMeta) []any {
	targets := make([]any, len(meta.pkCols))
	for i, cm := range meta.pkCols {
		switch cm.field.Type.Name {
		case "text", "varchar", "citext", "uuid":
			targets[i] = new(string)
		case "bigint":
			targets[i] = new(int64)
		case "int", "smallint":
			targets[i] = new(int32)
		default:
			targets[i] = new(string)
		}
	}
	return targets
}

// readPKScanTargets dereferences scan pointers into []any for cache
// keys or subsequent GET queries.
func readPKScanTargets(meta *entityMeta, targets []any) []any {
	out := make([]any, len(targets))
	for i, cm := range meta.pkCols {
		switch cm.field.Type.Name {
		case "text", "varchar", "citext", "uuid":
			out[i] = *(targets[i].(*string))
		case "bigint":
			out[i] = *(targets[i].(*int64))
		case "int", "smallint":
			out[i] = *(targets[i].(*int32))
		default:
			out[i] = *(targets[i].(*string))
		}
	}
	return out
}
