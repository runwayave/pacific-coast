package entity

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// customQueryMeta holds the pre-computed metadata for one custom query.
type customQueryMeta struct {
	query      *dsl.CustomQuery
	sql        string           // normalized: `$name` rewritten to `$N` via sqlparams
	inputCols  []dsl.QueryParam // declared inputs (for proto descriptors + type lookup)
	argOrder   []string         // input names in placeholder order; drives arg binding
	outputCols []dsl.QueryParam // populated when Output.Columns is set
	asEntity   bool             // true when Output.AsEntityID is set

	// Proto descriptors for the request/response messages.
	requestDesc  protoreflect.MessageDescriptor
	responseDesc protoreflect.MessageDescriptor
	rowDesc      protoreflect.MessageDescriptor // for column-output queries

	// The entity meta when asEntity is true (for scanRow).
	entityMeta *entityMeta

	timeoutMS int
}

// buildCustomQueryDescs builds proto descriptors for one custom query.
// The response is either a repeated entity or a repeated Row sub-message.
func buildCustomQueryDescs(cq *dsl.CustomQuery, ns string) (protoreflect.FileDescriptor, error) {
	goNS := goNamespace(ns)
	pkg := fmt.Sprintf("atlantis.%s.v1", goNS)
	fileName := fmt.Sprintf("atlantis/%s/v1/custom_%s_dynamic.proto", goNS, cq.Name)

	file := &descriptorpb.FileDescriptorProto{
		Name:    strPtr(fileName),
		Package: strPtr(pkg),
		Syntax:  strPtr("proto3"),
	}

	// Request message.
	reqMsg := &descriptorpb.DescriptorProto{
		Name: strPtr(cq.Name + "Request"),
	}
	for i, input := range cq.Inputs {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(input.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, input.Type)
		reqMsg.Field = append(reqMsg.Field, fd)
	}
	file.MessageType = append(file.MessageType, reqMsg)

	// Response message.
	respMsg := &descriptorpb.DescriptorProto{
		Name: strPtr(cq.Name + "Response"),
	}

	if len(cq.Output.Columns) > 0 {
		// Column-output: build a Row sub-message.
		rowMsg := &descriptorpb.DescriptorProto{
			Name: strPtr(cq.Name + "Response_Row"),
		}
		for i, col := range cq.Output.Columns {
			num := int32(i + 1)
			fd := &descriptorpb.FieldDescriptorProto{
				Name:   strPtr(col.Name),
				Number: &num,
			}
			label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			fd.Label = &label
			setProtoType(fd, col.Type)
			rowMsg.Field = append(rowMsg.Field, fd)
		}
		// Nest the Row inside the response.
		respMsg.NestedType = append(respMsg.NestedType, rowMsg)

		// Add repeated rows field.
		one := int32(1)
		repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		rowTypeName := fmt.Sprintf(".%s.%sResponse.%sResponse_Row", pkg, cq.Name, cq.Name)
		respMsg.Field = append(respMsg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr("rows"),
			Number:   &one,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(rowTypeName),
		})
	} else {
		// Entity-output: repeated entity field. We reference the entity
		// message by its fully qualified name. The entity's file descriptor
		// must be available in the resolver.
		one := int32(1)
		repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		// The entity's fully-qualified proto name.
		parts := splitEntityID(cq.Output.AsEntityID)
		entityNS := goNamespace(parts[0])
		entityTypeName := fmt.Sprintf(".atlantis.%s.v1.%s", entityNS, parts[1])
		respMsg.Field = append(respMsg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr("entities"),
			Number:   &one,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(entityTypeName),
		})
	}
	file.MessageType = append(file.MessageType, respMsg)

	// Check for timestamp dependencies.
	needsTimestamp := false
	for _, input := range cq.Inputs {
		if input.Type.Name == "timestamptz" || input.Type.Name == "date" {
			needsTimestamp = true
			break
		}
	}
	if !needsTimestamp {
		for _, col := range cq.Output.Columns {
			if col.Type.Name == "timestamptz" || col.Type.Name == "date" {
				needsTimestamp = true
				break
			}
		}
	}
	if needsTimestamp {
		file.Dependency = append(file.Dependency, "google/protobuf/timestamp.proto")
	}

	resolver := &fileResolver{
		files:  make(map[string]protoreflect.FileDescriptor),
		global: protoregistry.GlobalFiles,
	}

	fd, err := protodesc.NewFile(file, resolver)
	if err != nil {
		return nil, fmt.Errorf("building custom query descriptors for %s: %w", cq.Name, err)
	}
	return fd, nil
}

// makeCustomHandler captures the query key (ns:name) and looks up the
// customQueryMeta from the current snapshot at each request.
func makeCustomHandler(s *Server, queryKey string, ns string) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	goNS := goNamespace(ns)
	// Extract query name from key (format: "ns:queryName").
	queryName := queryKey
	if idx := len(ns) + 1; idx < len(queryKey) {
		queryName = queryKey[idx:]
	}
	fullMethod := fmt.Sprintf("/atlantis.%s.v1.CustomService/%s", goNS, queryName)

	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		snap := s.snapshot.Load()
		cqm, ok := snap.customMeta[queryKey]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "custom query %s not found in current schema", queryKey)
		}

		req := dynamicpb.NewMessage(cqm.requestDesc)
		if err := dec(req); err != nil {
			return nil, err
		}

		execHandler := func(ctx context.Context, _ any) (any, error) {
			return s.executeCustomQueryWithReq(ctx, cqm, req)
		}

		if interceptor == nil {
			return execHandler(ctx, nil)
		}
		info := &grpc.UnaryServerInfo{
			Server:     srv,
			FullMethod: fullMethod,
		}
		return interceptor(ctx, req, info, execHandler)
	}
}

func (s *Server) executeCustomQueryWithReq(ctx context.Context, cqm *customQueryMeta, req *dynamicpb.Message) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, cqm.timeoutMS)
	defer cancel()

	// Bind input args in placeholder order. `cqm.sql` has been rewritten
	// from `$name` to `$1, $2, ...` at snapshot-build time; `cqm.argOrder`
	// lists the input names in the order they first appear in the rewritten
	// SQL. We iterate it to bind values in the order PG expects. Inputs
	// declared but never referenced in the SQL are intentionally omitted —
	// no placeholder, no arg.
	args := make([]any, 0, len(cqm.argOrder))
	for _, name := range cqm.argOrder {
		fd := cqm.requestDesc.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return nil, fmt.Errorf("custom query %s: input %q not found in request descriptor", cqm.query.Name, name)
		}
		var inputType dsl.FieldType
		for _, in := range cqm.inputCols {
			if in.Name == name {
				inputType = in.Type
				break
			}
		}
		args = append(args, customBindValue(req, fd, inputType))
	}

	rows, err := s.pool.Query(ctx, cqm.sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resp := dynamicpb.NewMessage(cqm.responseDesc)

	if cqm.asEntity && cqm.entityMeta != nil {
		// Entity output: scan into entity messages.
		entitiesFD := cqm.responseDesc.Fields().ByName("entities")
		if entitiesFD == nil {
			return resp, nil
		}
		entities := resp.Mutable(entitiesFD).List()
		for rows.Next() {
			entity, err := scanRow(cqm.entityMeta, rows)
			if err != nil {
				return nil, err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
	} else if cqm.rowDesc != nil {
		// Column output: scan into Row messages.
		rowsFD := cqm.responseDesc.Fields().ByName("rows")
		if rowsFD == nil {
			return resp, nil
		}
		rowList := resp.Mutable(rowsFD).List()
		for rows.Next() {
			row, err := scanCustomRow(cqm, rows)
			if err != nil {
				return nil, err
			}
			rowList.Append(protoreflect.ValueOfMessage(row))
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	return resp, nil
}

func scanCustomRow(cqm *customQueryMeta, src interface{ Scan(dest ...any) error }) (*dynamicpb.Message, error) {
	cols := cqm.outputCols
	targets := make([]any, len(cols))

	for i, col := range cols {
		targets[i] = makeCustomScanTarget(col.Type)
	}

	if err := src.Scan(targets...); err != nil {
		return nil, err
	}

	msg := dynamicpb.NewMessage(cqm.rowDesc)
	for i, col := range cols {
		fd := cqm.rowDesc.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
		if fd == nil {
			continue
		}
		setCustomProtoField(msg, fd, col.Type, targets[i])
	}
	return msg, nil
}

func makeCustomScanTarget(t dsl.FieldType) any {
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		return new(string)
	case "bigint":
		return new(int64)
	case "int", "smallint":
		return new(int32)
	case "boolean":
		return new(bool)
	case "timestamptz", "date":
		return new(sql.NullTime)
	case "bytea", "jsonb":
		return new([]byte)
	}
	return new(string)
}

func setCustomProtoField(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, t dsl.FieldType, target any) {
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		msg.Set(fd, protoreflect.ValueOfString(*(target.(*string))))
	case "bigint":
		msg.Set(fd, protoreflect.ValueOfInt64(*(target.(*int64))))
	case "int", "smallint":
		msg.Set(fd, protoreflect.ValueOfInt32(*(target.(*int32))))
	case "boolean":
		msg.Set(fd, protoreflect.ValueOfBool(*(target.(*bool))))
	case "timestamptz", "date":
		v := *(target.(*sql.NullTime))
		if v.Valid {
			setTimestampField(msg, fd, v.Time)
		}
	case "bytea", "jsonb":
		v := *(target.(*[]byte))
		if v != nil {
			msg.Set(fd, protoreflect.ValueOfBytes(v))
		}
	default:
		msg.Set(fd, protoreflect.ValueOfString(*(target.(*string))))
	}
}

func customBindValue(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, t dsl.FieldType) any {
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
			return time.Time{}
		}
		sec := sub.Get(secFD).Int()
		nanoFD := sub.Descriptor().Fields().ByName("nanos")
		var nanos int32
		if nanoFD != nil {
			nanos = int32(sub.Get(nanoFD).Int())
		}
		return time.Unix(sec, int64(nanos)).UTC()
	case "bytea", "jsonb":
		return msg.Get(fd).Bytes()
	}
	return msg.Get(fd).String()
}

func splitEntityID(id string) [2]string {
	for i, c := range id {
		if c == '.' {
			return [2]string{id[:i], id[i+1:]}
		}
	}
	return [2]string{"", id}
}
