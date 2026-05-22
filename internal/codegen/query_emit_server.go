package codegen

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Emitters for the per-entity QueryX surface: FilterSpec package var,
// the QueryX RPC body, and the assembleQueryXFromPKs cache-hit helper.

const queryDefaultLimit = 100
const queryMaxLimit = 1000

// queryCacheTTLSeconds bounds the staleness window of a tier-2 hit. The
// generation counter retires entries on writes, so this is only the
// upper bound on a hit observed after a successful write that hasn't
// yet bumped the counter (a window of at most BumpDebounce).
const queryCacheTTLSeconds = 30

// emitFilterSpec writes a package-level var like:
//
//	var accountFilterSpec = query.FilterSpec{
//	    EntityID: "consumer.Account",
//	    TableName: "consumer_account",
//	    Fields: map[string]query.FieldSpec{
//	        "id":    {Column: "id", Kind: query.PredicateInt64},
//	        "email": {Column: "email", Kind: query.PredicateString},
//	        ...
//	    },
//	}
//
// Stamped once per entity at codegen time so QueryX doesn't rebuild the
// spec per request.
func emitFilterSpec(b *strings.Builder, e *dsl.Entity) {
	specVar := lowerFirst(e.Name) + "FilterSpec"
	fmt.Fprintf(b, "var %s = query.FilterSpec{\n", specVar)
	fmt.Fprintf(b, "\tEntityID:  %q,\n", e.ID())
	fmt.Fprintf(b, "\tTableName: %q,\n", tableNameFromID(e.ID()))
	b.WriteString("\tFields: map[string]query.FieldSpec{\n")
	fields := slices.Clone(e.Fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].ProtoNumber < fields[j].ProtoNumber
	})
	for _, f := range fields {
		kind, ok := predicateKindForField(f.Type)
		if !ok {
			continue
		}
		fmt.Fprintf(b, "\t\t%q: {Column: %q, Kind: query.%s},\n",
			f.Name, f.Name, kind)
	}
	b.WriteString("\t},\n")
	b.WriteString("}\n\n")
}

// emitProtoQueryMethod renders QueryX along with its tier-2 hit-path
// helper and the keyset cursor helpers. cache_skip bypasses Lookup but
// not Store; a caller asking for a strong read still warms cache.
//
// Pagination model: each request fetches limit+1 rows. If the extra
// row arrived, it gets dropped and its predecessor's cursor coords
// (the requested ORDER BY columns plus the PK as a tiebreaker) become
// the response's next_page_token. The next request echoes the token;
// the handler decodes it back to scalar args, builds a row-value
// comparison via runtime.KeysetPredicate, and AND-joins it onto WHERE.
func emitProtoQueryMethod(b *strings.Builder, e *dsl.Entity, srv string, inbound []inboundRef) {
	specVar := lowerFirst(e.Name) + "FilterSpec"
	selectList := strings.Join(quoteAll(fieldColumns(e)), ", ")
	table := qualifiedTable(e)
	pkName := primaryPKName(e)
	pkGoName := snakeToCamel(pkName)
	pkQuoted := quoteIdent(pkName)
	pkOrderFieldConst := fmt.Sprintf("pb.%sOrderField_%s_%s",
		e.Name, screamingSnake(e.Name+"OrderField"), strings.ToUpper(pkName))

	fmt.Fprintf(b, "const sqlQuery%sPrefix = `SELECT %s FROM %s`\n\n", e.Name, selectList, table)
	fmt.Fprintf(b, "const %sQueryCacheTTL = %d * time.Second\n\n", lowerFirst(e.Name), queryCacheTTLSeconds)

	// Build the per-field switch arms for keyset column resolution and
	// cursor extraction. Both share the same orderable-field set so they
	// stay in lockstep.
	var keysetCases, cursorCases strings.Builder
	for _, f := range e.Fields {
		if !orderableType(f.Type) {
			continue
		}
		caseLabel := fmt.Sprintf("pb.%sOrderField_%s_%s",
			e.Name, screamingSnake(e.Name+"OrderField"), strings.ToUpper(f.Name))
		fmt.Fprintf(&keysetCases, "\t\tcase %s:\n", caseLabel)
		fmt.Fprintf(&keysetCases, "\t\t\tident = %q\n", quoteIdent(f.Name))
		fmt.Fprintf(&keysetCases, "\t\t\tif %s == %s {\n", caseLabel, pkOrderFieldConst)
		keysetCases.WriteString("\t\t\t\tseenPK = true\n")
		keysetCases.WriteString("\t\t\t}\n")

		fmt.Fprintf(&cursorCases, "\t\tcase %s:\n", caseLabel)
		cursorCases.WriteString("\t\t\t" + cursorExtractorExpr(f, e) + "\n")
		fmt.Fprintf(&cursorCases, "\t\t\tif %s == %s {\n", caseLabel, pkOrderFieldConst)
		cursorCases.WriteString("\t\t\t\tseenPK = true\n")
		cursorCases.WriteString("\t\t\t}\n")
	}

	// Per-include dispatch arms. Each requested AccountInclude variant
	// maps to either an attach helper call (same-namespace, real source
	// message) or an Unimplemented response (cross-namespace or
	// composite-PK source).
	var includeDispatch strings.Builder
	if len(inbound) > 0 {
		includeDispatch.WriteString("\tfor _, inc := range req.GetIncludes() {\n")
		includeDispatch.WriteString("\t\tswitch inc {\n")
		includeEnum := fmt.Sprintf("%sInclude", e.Name)
		includeEnumPrefix := screamingSnake(includeEnum)
		for _, ref := range inbound {
			refNS, refEnt, ok := strings.Cut(ref.FromEntityID, ".")
			if !ok {
				continue
			}
			variantName := fmt.Sprintf("pb.%s_%s_%s_%s_BY_%s",
				includeEnum,
				includeEnumPrefix,
				screamingSnake(goNamespace(refNS)),
				screamingSnake(refEnt),
				strings.ToUpper(ref.FromField))
			fmt.Fprintf(&includeDispatch, "\t\tcase %s:\n", variantName)
			if !includableSource(ref, e) {
				fmt.Fprintf(&includeDispatch,
					"\t\t\treturn nil, status.Errorf(codes.Unimplemented, %q)\n",
					fmt.Sprintf("include %s.%s not supported (cross-namespace or composite-PK source)",
						ref.FromEntityID, ref.FromField))
				continue
			}
			helperName := includeAttachFuncName(e.Name, refEnt, ref.FromField)
			fmt.Fprintf(&includeDispatch, "\t\t\tif err := s.%s(ctx, resp.Entities); err != nil {\n", helperName)
			includeDispatch.WriteString("\t\t\t\treturn nil, err\n")
			includeDispatch.WriteString("\t\t\t}\n")
		}
		includeDispatch.WriteString("\t\t}\n")
		includeDispatch.WriteString("\t}\n")
	}

	var extrasInit strings.Builder
	extrasInit.WriteString("\textras := make([]string, 0, 2)\n")
	if e.SoftDeleteField != "" {
		fmt.Fprintf(&extrasInit, "\textras = append(extras, %q)\n",
			quoteIdent(e.SoftDeleteField)+" IS NULL")
	}
	partitionArgInsert := ""
	if e.PartitionField != "" {
		// $1 is reserved for the partition value, so TranslateFilter is
		// told placeholderStart=2.
		fmt.Fprintf(&extrasInit, "\tpartitionVal, err := runtime.CallerPartition(ctx)\n")
		extrasInit.WriteString("\tif err != nil {\n")
		extrasInit.WriteString("\t\treturn nil, err\n")
		extrasInit.WriteString("\t}\n")
		fmt.Fprintf(&extrasInit, "\textras = append(extras, %q)\n",
			quoteIdent(e.PartitionField)+" = $1")
		partitionArgInsert = "\targs = append([]any{partitionVal}, args...)\n"
	}
	startingPH := 1
	if e.PartitionField != "" {
		startingPH = 2
	}

	fmt.Fprintf(b, `// Query%s implements pb.%sServiceServer.
func (s *%s) Query%s(ctx context.Context, req *pb.Query%sRequest) (*pb.Query%sResponse, error) {
	ctx, cancel := runtime.Deadline(ctx, %sQueryTimeoutMS)
	defer cancel()

	limit := req.GetLimit()
	if limit <= 0 {
		limit = %d
	}
	if limit > %d {
		limit = %d
	}

	cacheGen, _ := s.QueryCache.Generation(ctx, %q)
	orderMsgs := make([]proto.Message, 0, len(req.GetOrder()))
	for _, o := range req.GetOrder() {
		orderMsgs = append(orderMsgs, o)
	}
	includeVals := make([]int32, 0, len(req.GetIncludes()))
	for _, inc := range req.GetIncludes() {
		includeVals = append(includeVals, int32(inc))
	}
	cacheHash, hashErr := queryresult.Hash(
		%q,
		req.GetFilter(),
		orderMsgs,
		req.GetLimit(),
		[]byte(req.GetPageToken()),
		req.GetFields(),
		includeVals,
		cacheGen,
	)
	cacheable := hashErr == nil && len(req.GetIncludes()) == 0

	if cacheable && !req.GetCacheSkip() {
		if entry, hit, _ := s.QueryCache.Lookup(ctx, %q, cacheHash); hit {
			return s.assembleQuery%sFromPKs(ctx, entry.PKs, entry.NextPageToken)
		}
	}

	keysetCols := s.build%sKeysetCols(req.GetOrder())
	cursorVals, err := runtime.DecodePageToken(req.GetPageToken(), %q)
	if err != nil {
		return nil, err
	}

%s
	where, args, _, err := query.TranslateFilter(%s, req.GetFilter().ProtoReflect(), %d, extras...)
	if err != nil {
		return nil, err
	}
%s
	if len(cursorVals) > 0 {
		cursorSQL, cursorArgs, err := runtime.KeysetPredicate(keysetCols, cursorVals, len(args)+1)
		if err != nil {
			return nil, err
		}
		if where == "" {
			where = cursorSQL
		} else {
			where = where + " AND " + cursorSQL
		}
		args = append(args, cursorArgs...)
	}

	var b strings.Builder
	b.WriteString(sqlQuery%sPrefix)
	if where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	b.WriteString(runtime.OrderByClauseFromKeyset(keysetCols))
	args = append(args, limit+1)
	fmt.Fprintf(&b, " LIMIT $%%d", len(args))
	sqlText := b.String()

	rows, err := s.DB.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resp := &pb.Query%sResponse{}
	for rows.Next() {
		entity := &pb.%s{}
		if err := scanInto%s(rows, entity); err != nil {
			return nil, err
		}
		resp.Entities = append(resp.Entities, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var nextPageToken string
	if int32(len(resp.Entities)) > limit {
		boundaryRow := resp.Entities[limit-1]
		resp.Entities = resp.Entities[:limit]
		cursorOut := extract%sCursor(boundaryRow, req.GetOrder())
		nextPageToken, _ = runtime.EncodePageToken(%q, cursorOut)
		resp.NextPageToken = nextPageToken
	}

%s
	if cacheable {
		pks := make([]string, 0, len(resp.Entities))
		for _, ent := range resp.Entities {
			pks = append(pks, fmt.Sprintf("%%v", ent.Get%s()))
		}
		_ = s.QueryCache.Store(ctx, %q, cacheHash, pks, nextPageToken, %sQueryCacheTTL)
	}
	return resp, nil
}

func (s *%s) assembleQuery%sFromPKs(ctx context.Context, pks []string, nextPageToken string) (*pb.Query%sResponse, error) {
	resp := &pb.Query%sResponse{NextPageToken: nextPageToken}
	if len(pks) == 0 {
		return resp, nil
	}
	rows, err := s.DB.Query(ctx, sqlBatchGet%s, pks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		entity := &pb.%s{}
		if err := scanInto%s(rows, entity); err != nil {
			return nil, err
		}
		resp.Entities = append(resp.Entities, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *%s) build%sKeysetCols(orders []*pb.%sOrderBy) []runtime.KeysetColumn {
	cols := make([]runtime.KeysetColumn, 0, len(orders)+1)
	seenPK := false
	for _, o := range orders {
		var ident string
		switch o.GetField() {
%s		default:
			continue
		}
		cols = append(cols, runtime.KeysetColumn{QuotedIdent: ident, Desc: o.GetDesc()})
	}
	if !seenPK {
		cols = append(cols, runtime.KeysetColumn{QuotedIdent: %q, Desc: false})
	}
	return cols
}

func extract%sCursor(ent *pb.%s, orders []*pb.%sOrderBy) []any {
	out := make([]any, 0, len(orders)+1)
	seenPK := false
	for _, o := range orders {
		switch o.GetField() {
%s		default:
			continue
		}
	}
	if !seenPK {
		out = append(out, ent.Get%s())
	}
	return out
}

`,
		e.Name, e.Name, // Query<E> implements pb.<E>ServiceServer (in doc)
		srv, e.Name, e.Name, e.Name, // func sig
		lowerFirst(e.Name), // <e>QueryTimeoutMS
		queryDefaultLimit,
		queryMaxLimit,
		queryMaxLimit,
		e.ID(), // Generation entity
		e.ID(), // Hash entity
		e.ID(), // Lookup entity
		e.Name, // assembleQuery<E>FromPKs (hit-path call)
		e.Name, // build<E>KeysetCols
		e.ID(), // DecodePageToken expectedEntityID
		extrasInit.String(),
		specVar, startingPH,
		partitionArgInsert,
		e.Name,                   // sqlQuery<E>Prefix
		e.Name,                   // resp type Query<E>Response
		e.Name,                   // pb.<E>{}
		e.Name,                   // scanInto<E>
		e.Name,                   // extract<E>Cursor (boundary call)
		e.ID(),                   // EncodePageToken entityID
		includeDispatch.String(), // include dispatch block
		pkGoName,                 // ent.Get<Pk>()
		e.ID(),                   // Store entity
		lowerFirst(e.Name),       // <e>QueryCacheTTL
		// assembleQuery<E>FromPKs helper
		srv, e.Name, e.Name, // func sig
		e.Name, // resp type Query<E>Response
		e.Name, // sqlBatchGet<E>
		e.Name, // pb.<E>{}
		e.Name, // scanInto<E>
		// build<E>KeysetCols
		srv, e.Name, e.Name,
		keysetCases.String(),
		pkQuoted,
		// extract<E>Cursor
		e.Name, e.Name, e.Name,
		cursorCases.String(),
		pkGoName,
	)

	emitIncludeAttachFuncs(b, e, srv, inbound)
}

// cursorExtractorExpr returns the Go fragment that appends one row's
// value for field f to the `out` slice. Timestamps come back from
// proto as *timestamppb.Timestamp and need .AsTime(); other scalars
// ride through unchanged.
func cursorExtractorExpr(f dsl.Field, _ *dsl.Entity) string {
	getter := "ent.Get" + snakeToCamel(f.Name) + "()"
	switch f.Type.Name {
	case "timestamptz", "date":
		return "out = append(out, " + getter + ".AsTime())"
	}
	return "out = append(out, " + getter + ")"
}

// includeAttachFuncName returns the Go method name that attaches one
// include relation. Stable across runs because both inputs come from
// the IR; reordering the inbound list doesn't rename the method.
func includeAttachFuncName(targetEntity, sourceEntity, fkField string) string {
	return fmt.Sprintf("attach%sInclude%sBy%s",
		targetEntity, sourceEntity, snakeToCamel(fkField))
}

// emitIncludeAttachFuncs writes one per-relation method per same-
// namespace, non-composite-PK source. Each method runs a single
// SELECT against the related table filtered by the FK, groups rows
// by FK value, and stamps each parent's matching include slot.
func emitIncludeAttachFuncs(b *strings.Builder, target *dsl.Entity, srv string, inbound []inboundRef) {
	pkGoName := snakeToCamel(primaryPKName(target))
	for _, ref := range inbound {
		if !includableSource(ref, target) {
			continue
		}
		src := ref.FromEntity
		if src == nil {
			continue
		}
		fkCol := ref.FromField
		fkColQuoted := quoteIdent(fkCol)
		srcCols := strings.Join(quoteAll(fieldColumns(src)), ", ")
		srcTable := qualifiedTable(src)
		fkGoName := snakeToCamel(fkCol)
		helperName := includeAttachFuncName(target.Name, src.Name, fkCol)
		sqlConstName := "sql" + strings.ToUpper(helperName[:1]) + helperName[1:]
		slotName := snakeToCamel(includeSlotName(src.Name, fkCol))

		fmt.Fprintf(b, "const %s = `SELECT %s FROM %s WHERE %s = ANY($1)`\n\n",
			sqlConstName, srcCols, srcTable, fkColQuoted)
		fmt.Fprintf(b, `func (s *%s) %s(ctx context.Context, parents []*pb.%s) error {
	if len(parents) == 0 {
		return nil
	}
	parentKeys := make([]string, 0, len(parents))
	for _, p := range parents {
		parentKeys = append(parentKeys, fmt.Sprintf("%%v", p.Get%s()))
	}
	rows, err := s.DB.Query(ctx, %s, parentKeys)
	if err != nil {
		return err
	}
	defer rows.Close()
	grouped := map[string][]*pb.%s{}
	for rows.Next() {
		child := &pb.%s{}
		if err := scanInto%s(rows, child); err != nil {
			return err
		}
		key := fmt.Sprintf("%%v", child.Get%s())
		grouped[key] = append(grouped[key], child)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range parents {
		key := fmt.Sprintf("%%v", p.Get%s())
		p.%s = grouped[key]
	}
	return nil
}

`,
			srv, helperName, target.Name,
			pkGoName,
			sqlConstName,
			src.Name,
			src.Name,
			src.Name,
			fkGoName,
			pkGoName,
			slotName,
		)
	}
}

// primaryPKName returns the snake-case name of the entity's primary key
// column for the keyset cursor's PK tiebreaker. Single-PK entities return
// the lone PK field; composite-PK entities return the first declared PK
// column — the cursor includes every column in the ORDER BY plus this
// PK component as the final tiebreaker, so a multi-column composite PK
// is reachable by augmenting the ORDER BY with the remaining columns
// (see internal/runtime/pagination.go for how the cursor extends across
// arbitrary order-arity).
//
// The "id" fallback is a defensive guard against malformed IR slipping
// past validation; in a healthy IR every entity has a PrimaryField or
// CompositePK declaration.
func primaryPKName(e *dsl.Entity) string {
	if pk := e.PrimaryField(); pk != nil {
		return pk.Name
	}
	if len(e.CompositePK) > 0 {
		return e.CompositePK[0]
	}
	return "id"
}

// predicateKindForField maps a DSL field type to the query.PredicateKind
// constant name. The constants are defined in
// internal/codegen/query/spec.go. Returns ("", false) when the type
// isn't filterable.
//
// Mirrors predicateMessageForField in query_emit.go — keep both in sync;
// adding a new filterable type means a new arm here, in query_emit.go,
// AND a new switch arm in translator.translatePredicate.
func predicateKindForField(t dsl.FieldType) (string, bool) {
	if t.Array {
		return "", false
	}
	switch t.Name {
	case "text", "varchar", "citext", "uuid":
		return "PredicateString", true
	case "numeric":
		return "PredicateNumeric", true
	case "int", "smallint":
		return "PredicateInt32", true
	case "bigint":
		return "PredicateInt64", true
	case "boolean":
		return "PredicateBool", true
	case "timestamptz", "date":
		return "PredicateTimestamp", true
	case "jsonb", "bytea":
		return "PredicateBytes", true
	}
	return "", false
}
