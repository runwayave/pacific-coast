// Custom-query and procedure codegen.
//
// One CustomService is emitted per namespace, regardless of which .pc
// file each query / procedure originally lived in. Grouping by
// namespace keeps the per-namespace package layout uniform with the
// rest of the generated code (gen/go/server/<ns>/, clients/go/client/<ns>/,
// atlantis/<ns>/v1/) and lets a future BSR push expose one stable
// custom-service contract per namespace.
//
// Queries and procedures share the service block but have distinct
// response shapes:
//
//   - Query with `output as <Entity>` returns `repeated <Entity> rows`.
//   - Query with `output { col: type, ... }` returns a nested `Row`
//     message containing each declared column.
//   - Procedure returns `int64 rows_affected` — the total of every
//     mutation's RowsAffected across the steps. Engineers wanting
//     per-step counts should split the procedure.
//
// The server emitter builds the per-query SQL at codegen time by
// rewriting DSL-style `$ident` placeholders to PG positional `$N` and
// recording the input-name order so the request shell binds args in
// the same order. Typed steps inside procedures are lowered to
// hand-built SQL fragments (UPDATE / DELETE / INSERT) so the codegen
// stays free of any runtime DSL → SQL translator.

package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/codegen/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// EmitCustomProto renders one `custom.proto` per namespace that
// contains any custom queries or procedures. Returns an empty slice
// when no namespace has custom declarations.
func EmitCustomProto(ir *dsl.IR) ([]ProtoFile, error) {
	if ir == nil {
		return nil, fmt.Errorf("EmitCustomProto: ir is required")
	}
	groups := groupCustomByNamespace(ir)
	if len(groups) == 0 {
		return nil, nil
	}
	out := make([]ProtoFile, 0, len(groups))
	for _, ns := range sortedKeys(groups) {
		g := groups[ns]
		f, err := emitCustomProtoForNamespace(ir, ns, g)
		if err != nil {
			return nil, fmt.Errorf("custom proto %s: %w", ns, err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// EmitCustomServer renders one `custom_server.go` per namespace that
// contains any custom decls. The aggregator in emitGoServerRegister
// adds the matching `RegisterCustomServiceServer` lines, so the
// resulting binary mounts the service automatically.
func EmitCustomServer(ir *dsl.IR) ([]GoFile, error) {
	if ir == nil {
		return nil, fmt.Errorf("EmitCustomServer: ir is required")
	}
	groups := groupCustomByNamespace(ir)
	if len(groups) == 0 {
		return nil, nil
	}
	out := make([]GoFile, 0, len(groups))
	for _, ns := range sortedKeys(groups) {
		g := groups[ns]
		f, err := emitCustomServerForNamespace(ir, ns, g)
		if err != nil {
			return nil, fmt.Errorf("custom server %s: %w", ns, err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// EmitCustomClient renders one `custom_client.go` per namespace with
// the typed wrapper around the buf-generated CustomServiceClient. The
// wrapper is symmetric with the per-entity clients — one level of
// indirection that lets cross-cutting concerns (retries, metrics)
// land here without touching buf output.
func EmitCustomClient(ir *dsl.IR) ([]GoFile, error) {
	if ir == nil {
		return nil, fmt.Errorf("EmitCustomClient: ir is required")
	}
	groups := groupCustomByNamespace(ir)
	if len(groups) == 0 {
		return nil, nil
	}
	out := make([]GoFile, 0, len(groups))
	for _, ns := range sortedKeys(groups) {
		g := groups[ns]
		f, err := emitCustomClientForNamespace(ns, g)
		if err != nil {
			return nil, fmt.Errorf("custom client %s: %w", ns, err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// customGroup carries all the queries and procedures rooted in one
// namespace (the namespace of each declaration's `for` target). Within
// a group, queries and procedures share the same CustomService block.
type customGroup struct {
	Queries    []*dsl.CustomQuery
	Procedures []*dsl.CustomProcedure
}

// groupCustomByNamespace partitions the IR's custom decls by their
// owner's namespace. Stable sort: alphabetical by name within each
// namespace so codegen output is deterministic across runs.
func groupCustomByNamespace(ir *dsl.IR) map[string]*customGroup {
	out := map[string]*customGroup{}
	for i := range ir.Queries {
		q := &ir.Queries[i]
		ns := namespaceFromID(q.Owner)
		if _, ok := out[ns]; !ok {
			out[ns] = &customGroup{}
		}
		out[ns].Queries = append(out[ns].Queries, q)
	}
	for i := range ir.Procedures {
		p := &ir.Procedures[i]
		ns := namespaceFromID(p.Owner)
		if _, ok := out[ns]; !ok {
			out[ns] = &customGroup{}
		}
		out[ns].Procedures = append(out[ns].Procedures, p)
	}
	for _, g := range out {
		sort.Slice(g.Queries, func(i, j int) bool { return g.Queries[i].Name < g.Queries[j].Name })
		sort.Slice(g.Procedures, func(i, j int) bool { return g.Procedures[i].Name < g.Procedures[j].Name })
	}
	return out
}

func namespaceFromID(id string) string {
	if i := strings.IndexByte(id, '.'); i >= 0 {
		return id[:i]
	}
	return ""
}

func sortedKeys(m map[string]*customGroup) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- proto emission ----

func emitCustomProtoForNamespace(ir *dsl.IR, ns string, g *customGroup) (ProtoFile, error) {
	var b strings.Builder
	b.WriteString("// Code generated by tidectl. DO NOT EDIT.\n\n")
	b.WriteString("syntax = \"proto3\";\n\n")
	fmt.Fprintf(&b, "package atlantis.%s.v1;\n\n", goNamespace(ns))

	// Imports: timestamp (for any timestamptz input/output) + one
	// per cross-namespace `output as <Entity>` reference. The same-
	// namespace entity imports are picked up automatically since
	// atlantis.<ns>.v1.<Entity> is in the same proto package.
	imports := buildCustomProtoImports(ir, ns, g)
	for _, imp := range imports {
		fmt.Fprintf(&b, "import %q;\n", imp)
	}
	if len(imports) > 0 {
		b.WriteString("\n")
	}

	// Service block: one RPC per query, one per procedure. Sorted by
	// name so the wire surface is determinism-friendly.
	b.WriteString("service CustomService {\n")
	for _, q := range g.Queries {
		fmt.Fprintf(&b, "  rpc %s(%sRequest) returns (%sResponse);\n", q.Name, q.Name, q.Name)
	}
	for _, p := range g.Procedures {
		fmt.Fprintf(&b, "  rpc %s(%sRequest) returns (%sResponse);\n", p.Name, p.Name, p.Name)
	}
	b.WriteString("}\n\n")

	// Per-query/procedure request + response messages.
	for _, q := range g.Queries {
		if err := emitCustomQueryMessages(&b, ir, ns, q); err != nil {
			return ProtoFile{}, err
		}
	}
	for _, p := range g.Procedures {
		if err := emitCustomProcedureMessages(&b, p); err != nil {
			return ProtoFile{}, err
		}
	}

	path := fmt.Sprintf("atlantis/%s/v1/custom.proto", goNamespace(ns))
	return ProtoFile{Path: path, Content: b.String()}, nil
}

func buildCustomProtoImports(ir *dsl.IR, ns string, g *customGroup) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	// Timestamp import only when at least one input or output column is
	// a timestamptz/date type. Cheap to compute, keeps the proto file
	// uncluttered when not needed.
	if customGroupHasTimestamp(g) {
		add("google/protobuf/timestamp.proto")
	}
	for _, q := range g.Queries {
		if q.Output.AsEntityID != "" {
			otherNS := namespaceFromID(q.Output.AsEntityID)
			if otherNS != ns {
				other := ir.LookupEntity(q.Output.AsEntityID)
				if other != nil {
					add(fmt.Sprintf("atlantis/%s/v1/%s.proto", goNamespace(otherNS), snakeCase(other.Name)))
				}
			} else {
				other := ir.LookupEntity(q.Output.AsEntityID)
				if other != nil {
					add(fmt.Sprintf("atlantis/%s/v1/%s.proto", goNamespace(otherNS), snakeCase(other.Name)))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func customGroupHasTimestamp(g *customGroup) bool {
	hasTS := func(t dsl.FieldType) bool {
		return t.Name == "timestamptz" || t.Name == "date"
	}
	check := func(params []dsl.QueryParam) bool {
		for _, p := range params {
			if hasTS(p.Type) {
				return true
			}
		}
		return false
	}
	for _, q := range g.Queries {
		if check(q.Inputs) || check(q.Output.Columns) {
			return true
		}
	}
	for _, p := range g.Procedures {
		if check(p.Inputs) {
			return true
		}
	}
	return false
}

func emitCustomQueryMessages(b *strings.Builder, ir *dsl.IR, ns string, q *dsl.CustomQuery) error {
	// Request message.
	fmt.Fprintf(b, "message %sRequest {\n", q.Name)
	for i, in := range q.Inputs {
		pt, err := protoFieldType(in.Type)
		if err != nil {
			return fmt.Errorf("query %s input %s: %w", q.Name, in.Name, err)
		}
		fmt.Fprintf(b, "  %s %s = %d;\n", pt, in.Name, i+1)
	}
	b.WriteString("}\n\n")

	// Response message.
	switch {
	case q.Output.AsEntityID != "":
		other := ir.LookupEntity(q.Output.AsEntityID)
		if other == nil {
			return fmt.Errorf("query %s: output entity %s not found", q.Name, q.Output.AsEntityID)
		}
		ref := other.Name
		// Cross-namespace reference uses the fully-qualified proto
		// package name so the proto compiler picks the right message.
		if other.Namespace != ns {
			ref = fmt.Sprintf("atlantis.%s.v1.%s", goNamespace(other.Namespace), other.Name)
		}
		fmt.Fprintf(b, "message %sResponse {\n  repeated %s rows = 1;\n}\n\n", q.Name, ref)
	default:
		// Synthetic Row message inside the response.
		fmt.Fprintf(b, "message %sResponse {\n", q.Name)
		fmt.Fprintf(b, "  message Row {\n")
		for i, c := range q.Output.Columns {
			pt, err := protoFieldType(c.Type)
			if err != nil {
				return fmt.Errorf("query %s output %s: %w", q.Name, c.Name, err)
			}
			fmt.Fprintf(b, "    %s %s = %d;\n", pt, c.Name, i+1)
		}
		fmt.Fprintf(b, "  }\n  repeated Row rows = 1;\n}\n\n")
	}
	return nil
}

func emitCustomProcedureMessages(b *strings.Builder, p *dsl.CustomProcedure) error {
	fmt.Fprintf(b, "message %sRequest {\n", p.Name)
	for i, in := range p.Inputs {
		pt, err := protoFieldType(in.Type)
		if err != nil {
			return fmt.Errorf("procedure %s input %s: %w", p.Name, in.Name, err)
		}
		fmt.Fprintf(b, "  %s %s = %d;\n", pt, in.Name, i+1)
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(b, "message %sResponse {\n  int64 rows_affected = 1;\n}\n\n", p.Name)
	return nil
}

// ---- Go server emission ----

func emitCustomServerForNamespace(ir *dsl.IR, ns string, g *customGroup) (GoFile, error) {
	var b strings.Builder
	b.WriteString("// Code generated by tidectl. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", goNamespace(ns))

	needsPgvector, needsDatabaseSQL := customGroupImports(g)

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n")
	if needsDatabaseSQL {
		b.WriteString("\t\"database/sql\"\n")
	}
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"time\"\n\n")
	if needsPgvector {
		b.WriteString("\tpgvector \"github.com/pgvector/pgvector-go\"\n\n")
	}
	fmt.Fprintf(&b, "\tpb \"github.com/rachitkumar205/atlantis-go/pb/atlantis/%s/v1\"\n", goNamespace(ns))
	b.WriteString("\t\"github.com/rachitkumar205/atlantis/internal/runtime\"\n")
	b.WriteString(")\n\n")

	b.WriteString("var _ = fmt.Sprintf\n")
	b.WriteString("var _ = time.Time{}\n\n")

	// Per-package deadline budgets for the custom-service surface. The
	// numbers mirror the entity-server default (2s for reads) while
	// procedures get a longer slot since they hold a tx open across
	// multiple statements. Both are overridable per-query in a future
	// DSL extension; today they're constants stamped per namespace.
	b.WriteString("const (\n")
	b.WriteString("\tcustomQueryTimeoutMS = 2000\n")
	b.WriteString("\tcustomProcTimeoutMS  = 5000\n")
	b.WriteString(")\n\n")

	// Server struct + constructor. The CustomService doesn't use the
	// QueryCache the entity servers thread through because custom
	// queries don't share their result shape with QueryX's PK-list
	// cache. Caching custom-query results is a separate layer that
	// can be added without changing this signature.
	b.WriteString("// CustomServer implements pb.CustomServiceServer for this namespace.\n")
	b.WriteString("type CustomServer struct {\n")
	b.WriteString("\tpb.UnimplementedCustomServiceServer\n")
	b.WriteString("\tDB     runtime.Pool\n")
	b.WriteString("\tOutbox runtime.Outbox\n")
	b.WriteString("}\n\n")

	b.WriteString("// NewCustomServer constructs the handler with its runtime dependencies.\n")
	b.WriteString("func NewCustomServer(db runtime.Pool, outbox runtime.Outbox) *CustomServer {\n")
	b.WriteString("\treturn &CustomServer{DB: db, Outbox: outbox}\n")
	b.WriteString("}\n\n")

	// Per-query/procedure method.
	for _, q := range g.Queries {
		if err := emitCustomQueryHandler(&b, ir, q); err != nil {
			return GoFile{}, fmt.Errorf("query %s: %w", q.Name, err)
		}
	}
	for _, p := range g.Procedures {
		if err := emitCustomProcedureHandler(&b, ir, p); err != nil {
			return GoFile{}, fmt.Errorf("procedure %s: %w", p.Name, err)
		}
	}

	fmt.Fprintf(&b, "var _ pb.CustomServiceServer = (*CustomServer)(nil)\n")

	path := fmt.Sprintf("gen/go/server/%s/custom_server.go", goNamespace(ns))
	return GoFile{Path: path, Content: b.String()}, nil
}

func emitCustomQueryHandler(b *strings.Builder, ir *dsl.IR, q *dsl.CustomQuery) error {
	// Rewrite the DSL SQL body so PG sees positional placeholders and
	// the args slice is built in the right order.
	normSQL, argOrder, err := normalizeSQLParams(q.SQL, q.Inputs)
	if err != nil {
		return err
	}
	// Bake the SQL into a const so the linter and EXPLAIN line up on
	// one source. Backticks are escaped by replacing them with a
	// quoted-concatenation, but the DSL grammar already rejects them
	// inside raw bodies (the lexer would have lost track of the
	// closing `}` if a backtick had broken parsing).
	fmt.Fprintf(b, "const sqlCustom_%s = `%s`\n\n", q.Name, normSQL)

	// Method signature + deadline.
	fmt.Fprintf(b, "// %s implements pb.CustomServiceServer.\n", q.Name)
	fmt.Fprintf(b, "func (s *CustomServer) %s(ctx context.Context, req *pb.%sRequest) (*pb.%sResponse, error) {\n", q.Name, q.Name, q.Name)
	b.WriteString("\tctx, cancel := runtime.Deadline(ctx, customQueryTimeoutMS)\n")
	b.WriteString("\tdefer cancel()\n\n")

	// Bind args. coltype wraps vectors through pgvector.NewVector, nullable
	// scalars through their runtime helpers, etc. — so pgx sees the right
	// shape for every input regardless of DSL type.
	if len(argOrder) > 0 {
		b.WriteString("\targs := []any{\n")
		for _, name := range argOrder {
			fmt.Fprintf(b, "\t\t%s,\n", customArgBindExpr(name, q.Inputs))
		}
		b.WriteString("\t}\n\n")
	} else {
		b.WriteString("\tvar args []any\n\n")
	}

	fmt.Fprintf(b, "\trows, err := s.DB.Query(ctx, sqlCustom_%s, args...)\n", q.Name)
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\tdefer rows.Close()\n\n")

	// Scan loop varies by output shape.
	switch {
	case q.Output.AsEntityID != "":
		other := ir.LookupEntity(q.Output.AsEntityID)
		if other == nil {
			return fmt.Errorf("output entity %s not found", q.Output.AsEntityID)
		}
		// For now, the entity scan helper (scanInto<Entity>) lives in
		// the entity's per-namespace package. When the output entity
		// is in this namespace, we can call it directly. Cross-
		// namespace `as <Entity>` is rarer; if a callsite needs it,
		// we'll add a public helper. For now reject it loudly during
		// codegen so the failure is at build time, not runtime.
		if other.Namespace != namespaceFromID(q.Owner) {
			return fmt.Errorf("query %s: cross-namespace `output as %s` not supported: the scan helper is private to the target entity's package", q.Name, q.Output.AsEntityID)
		}
		fmt.Fprintf(b, "\tresp := &pb.%sResponse{}\n", q.Name)
		fmt.Fprintf(b, "\tfor rows.Next() {\n")
		fmt.Fprintf(b, "\t\trow := &pb.%s{}\n", other.Name)
		fmt.Fprintf(b, "\t\tif err := scanInto%s(rows, row); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", other.Name)
		fmt.Fprintf(b, "\t\tresp.Rows = append(resp.Rows, row)\n")
		fmt.Fprintf(b, "\t}\n")
	default:
		fmt.Fprintf(b, "\tresp := &pb.%sResponse{}\n", q.Name)
		fmt.Fprintf(b, "\tfor rows.Next() {\n")
		fmt.Fprintf(b, "\t\trow := &pb.%sResponse_Row{}\n", q.Name)
		// Three buffers because the decl block, the rows.Scan call, and
		// the post-scan assignment loop emit into different parts of
		// the handler body — coltype hands back the three pieces in
		// one call so vectors/arrays/nullables stay consistent.
		decls := make([]string, len(q.Output.Columns))
		targets := make([]string, len(q.Output.Columns))
		assigns := make([]string, len(q.Output.Columns))
		for i, c := range q.Output.Columns {
			local := fmt.Sprintf("v%d", i)
			protoField := "row." + snakeToCamel(c.Name)
			decls[i], targets[i], assigns[i] = coltype.ScanFragments(c.Type, true, local, protoField)
		}
		for _, d := range decls {
			fmt.Fprintf(b, "\t\t%s\n", d)
		}
		fmt.Fprintf(b, "\t\tif err := rows.Scan(%s); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", strings.Join(targets, ", "))
		for _, a := range assigns {
			fmt.Fprintf(b, "\t\t%s\n", a)
		}
		fmt.Fprintf(b, "\t\tresp.Rows = append(resp.Rows, row)\n")
		fmt.Fprintf(b, "\t}\n")
	}

	b.WriteString("\tif err := rows.Err(); err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\treturn resp, nil\n}\n\n")
	return nil
}

// customGroupImports reports which optional imports the generated
// custom-server file needs. pgvector flows in for any column or input
// typed vector(N); database/sql flows in when a nullable scalar column
// needs sql.NullX as its scan local. Walks every query input + output
// column and every procedure input so the imports stay in sync with
// the type set actually consumed.
func customGroupImports(g *customGroup) (needsPgvector, needsDatabaseSQL bool) {
	visit := func(t dsl.FieldType, notNull bool) {
		if coltype.NeedsPgvector(t) {
			needsPgvector = true
		}
		if coltype.NeedsDatabaseSQL(t, notNull) {
			needsDatabaseSQL = true
		}
	}
	for _, q := range g.Queries {
		for _, in := range q.Inputs {
			visit(in.Type, true)
		}
		for _, c := range q.Output.Columns {
			visit(c.Type, true)
		}
	}
	for _, p := range g.Procedures {
		for _, in := range p.Inputs {
			visit(in.Type, true)
		}
	}
	return
}

func emitCustomProcedureHandler(b *strings.Builder, ir *dsl.IR, p *dsl.CustomProcedure) error {
	fmt.Fprintf(b, "// %s implements pb.CustomServiceServer.\n", p.Name)
	fmt.Fprintf(b, "func (s *CustomServer) %s(ctx context.Context, req *pb.%sRequest) (*pb.%sResponse, error) {\n", p.Name, p.Name, p.Name)
	b.WriteString("\tctx, cancel := runtime.Deadline(ctx, customProcTimeoutMS)\n")
	b.WriteString("\tdefer cancel()\n\n")

	b.WriteString("\ttx, err := s.DB.BeginTx(ctx)\n")
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	b.WriteString("\tdefer func() { _ = tx.Rollback(context.Background()) }()\n\n")

	b.WriteString("\tvar rowsAffected int64\n")

	// Inputs map: name → ordinal (for procedure-level $name resolution).
	inputOrdinal := make(map[string]int, len(p.Inputs))
	for i, in := range p.Inputs {
		inputOrdinal[in.Name] = i + 1
	}

	// Per-step emission.
	touchedSet := map[string]bool{}
	for i, step := range p.Steps {
		switch {
		case step.Typed != nil:
			if err := emitProcedureTypedStep(b, ir, step.Typed, inputOrdinal, p.Inputs, i, p.Name); err != nil {
				return err
			}
			touchedSet[step.Typed.TargetID] = true
		case step.Raw != nil:
			if err := emitProcedureRawStep(b, step.Raw, inputOrdinal, p.Inputs, i); err != nil {
				return err
			}
			for _, t := range step.Raw.Touches {
				touchedSet[t] = true
			}
		}
	}

	// Outbox bumps for every touched entity, sorted for deterministic
	// emission. Bumps fire inside the tx so the commit makes the
	// pre/post-write state atomic with the cache-invalidation enqueue.
	bumps := make([]string, 0, len(touchedSet))
	for t := range touchedSet {
		bumps = append(bumps, t)
	}
	sort.Strings(bumps)
	if len(bumps) > 0 {
		b.WriteString("\n")
		for _, t := range bumps {
			fmt.Fprintf(b, "\tif err := s.Outbox.EnqueueGenerationBump(ctx, tx, %q); err != nil {\n\t\treturn nil, err\n\t}\n", t)
		}
	}

	b.WriteString("\n\tif err := tx.Commit(ctx); err != nil {\n\t\treturn nil, err\n\t}\n")
	fmt.Fprintf(b, "\treturn &pb.%sResponse{RowsAffected: rowsAffected}, nil\n}\n\n", p.Name)
	return nil
}

func emitProcedureTypedStep(b *strings.Builder, ir *dsl.IR, ts *dsl.TypedStepIR, inputOrdinal map[string]int, inputs []dsl.QueryParam, idx int, procName string) error {
	target := ir.LookupEntity(ts.TargetID)
	if target == nil {
		return fmt.Errorf("step %d: unknown target %s", idx+1, ts.TargetID)
	}
	tableName := fmt.Sprintf("%q.%q", "atlantis", target.Namespace+"_"+snakeCase(target.Name))

	// Render the SQL fragment and the corresponding args slice. The
	// renderer is small by design — the DSL grammar for typed-step
	// expressions is tiny (literal / arg / field / binary), so the
	// SQL emission is straightforward.
	var sql strings.Builder
	args := []string{}
	placeholderIdx := 0
	nextPH := func() string {
		placeholderIdx++
		return fmt.Sprintf("$%d", placeholderIdx)
	}

	switch ts.Verb {
	case "update":
		fmt.Fprintf(&sql, "UPDATE %s SET ", tableName)
		for i, a := range ts.Assigns {
			if i > 0 {
				sql.WriteString(", ")
			}
			fmt.Fprintf(&sql, "%q = ", a.Field)
			renderExprToSQL(&sql, a.Value, nextPH, &args, inputs)
		}
		if ts.Where != nil {
			sql.WriteString(" WHERE ")
			renderExprToSQL(&sql, ts.Where, nextPH, &args, inputs)
		}
	case "delete":
		fmt.Fprintf(&sql, "DELETE FROM %s", tableName)
		if ts.Where != nil {
			sql.WriteString(" WHERE ")
			renderExprToSQL(&sql, ts.Where, nextPH, &args, inputs)
		}
	case "insert":
		fmt.Fprintf(&sql, "INSERT INTO %s (", tableName)
		for i, a := range ts.Assigns {
			if i > 0 {
				sql.WriteString(", ")
			}
			fmt.Fprintf(&sql, "%q", a.Field)
		}
		sql.WriteString(") VALUES (")
		for i, a := range ts.Assigns {
			if i > 0 {
				sql.WriteString(", ")
			}
			renderExprToSQL(&sql, a.Value, nextPH, &args, inputs)
		}
		sql.WriteString(")")
	default:
		return fmt.Errorf("step %d: unknown verb %q", idx+1, ts.Verb)
	}

	// Emit the SQL constant + the exec call. Each typed step gets its
	// own constant for grep-ability and EXPLAIN-ability.
	constName := fmt.Sprintf("sqlProc_%s_step%d", procName, idx+1)
	fmt.Fprintf(b, "\tconst %s = `%s`\n", constName, sql.String())
	if len(args) > 0 {
		fmt.Fprintf(b, "\ttag%d, err := tx.Exec(ctx, %s, %s)\n", idx, constName, strings.Join(args, ", "))
	} else {
		fmt.Fprintf(b, "\ttag%d, err := tx.Exec(ctx, %s)\n", idx, constName)
	}
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	fmt.Fprintf(b, "\trowsAffected += tag%d.RowsAffected()\n\n", idx)
	return nil
}

// renderExprToSQL walks one IR expression tree, appending SQL fragments
// to `sql` and Go expressions to `args` (as side-effects via shared
// pointers). The expression grammar is tight enough that no recursive
// state needs to flow back — only the next placeholder index, which
// the caller manages.
func renderExprToSQL(sql *strings.Builder, e *dsl.ExprIR, nextPH func() string, args *[]string, inputs []dsl.QueryParam) {
	if e == nil {
		return
	}
	switch e.Kind {
	case dsl.ExprArg:
		sql.WriteString(nextPH())
		*args = append(*args, customArgBindExpr(e.ArgName, inputs))
	case dsl.ExprField:
		fmt.Fprintf(sql, "%q", e.Field)
	case dsl.ExprLiteralStr:
		sql.WriteString(nextPH())
		*args = append(*args, fmt.Sprintf("%q", e.LitStr))
	case dsl.ExprLiteralInt:
		fmt.Fprintf(sql, "%d", e.LitInt)
	case dsl.ExprLiteralBool:
		fmt.Fprintf(sql, "%t", e.LitBool)
	case dsl.ExprLiteralNow:
		// Inline `now()` so PG evaluates it at statement time rather
		// than the codegen-emitted time. This matches how
		// `default now()` already works for entity columns.
		sql.WriteString("now()")
	case dsl.ExprBinary:
		// Parens around children would be safer for OR / NOT, but the
		// typed-step grammar only allows `and` at the top level and
		// comparisons inside — no ambiguity exists yet.
		renderExprToSQL(sql, e.Left, nextPH, args, inputs)
		switch e.Op {
		case "and":
			sql.WriteString(" AND ")
		case "=":
			sql.WriteString(" = ")
		case "!=":
			sql.WriteString(" <> ")
		default:
			sql.WriteString(" " + e.Op + " ")
		}
		renderExprToSQL(sql, e.Right, nextPH, args, inputs)
	}
}

// customArgBindExpr renders the Go expression that pulls one $arg off
// the request message and wraps it for pgx bind. Routes through
// coltype so vector(N) wraps as pgvector.NewVector, timestamps convert
// through runtime.ProtoToTime, etc. — the same surface entity CRUD
// uses for bind.
//
// Inputs are always not-null today; pass "" for the nullable pointer
// slot since the nullable branches aren't reached. When the DSL grows
// nullable input markers, switch to looking up the input's NotNull
// bit and threading the proto pointer in.
func customArgBindExpr(name string, inputs []dsl.QueryParam) string {
	var t dsl.FieldType
	for _, in := range inputs {
		if in.Name == name {
			t = in.Type
			break
		}
	}
	getter := "req.Get" + snakeToCamel(name) + "()"
	return coltype.BindExpr(t, true, getter, "")
}

func emitProcedureRawStep(b *strings.Builder, raw *dsl.RawSQLIR, inputOrdinal map[string]int, inputs []dsl.QueryParam, idx int) error {
	normSQL, argOrder, err := normalizeSQLParamsRaw(raw.SQL, inputOrdinal)
	if err != nil {
		return err
	}
	constName := fmt.Sprintf("sqlProcRaw_step%d", idx+1)
	fmt.Fprintf(b, "\tconst %s = `%s`\n", constName, normSQL)
	if len(argOrder) > 0 {
		fmt.Fprintf(b, "\ttag%d, err := tx.Exec(ctx, %s, %s)\n", idx, constName, strings.Join(rawArgGetters(argOrder, inputs), ", "))
	} else {
		fmt.Fprintf(b, "\ttag%d, err := tx.Exec(ctx, %s)\n", idx, constName)
	}
	b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	fmt.Fprintf(b, "\trowsAffected += tag%d.RowsAffected()\n\n", idx)
	return nil
}

func rawArgGetters(order []string, inputs []dsl.QueryParam) []string {
	out := make([]string, len(order))
	for i, name := range order {
		out[i] = customArgBindExpr(name, inputs)
	}
	return out
}

// ---- Go client emission ----

func emitCustomClientForNamespace(ns string, g *customGroup) (GoFile, error) {
	var b strings.Builder
	b.WriteString("// Code generated by tidectl. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", goNamespace(ns))

	b.WriteString("import (\n")
	b.WriteString("\t\"context\"\n\n")
	b.WriteString("\t\"google.golang.org/grpc\"\n\n")
	fmt.Fprintf(&b, "\tpb \"github.com/rachitkumar205/atlantis-go/pb/atlantis/%s/v1\"\n", goNamespace(ns))
	b.WriteString(")\n\n")

	b.WriteString("// CustomClient is the typed surface for the namespace's custom queries and procedures.\n")
	b.WriteString("type CustomClient interface {\n")
	for _, q := range g.Queries {
		fmt.Fprintf(&b, "\t%s(ctx context.Context, req *pb.%sRequest, opts ...grpc.CallOption) (*pb.%sResponse, error)\n", q.Name, q.Name, q.Name)
	}
	for _, p := range g.Procedures {
		fmt.Fprintf(&b, "\t%s(ctx context.Context, req *pb.%sRequest, opts ...grpc.CallOption) (*pb.%sResponse, error)\n", p.Name, p.Name, p.Name)
	}
	b.WriteString("}\n\n")

	b.WriteString("type customClient struct {\n")
	b.WriteString("\tinner pb.CustomServiceClient\n")
	b.WriteString("}\n\n")

	b.WriteString("// NewCustomClient constructs a typed CustomService client from a live grpc.ClientConnInterface.\n")
	b.WriteString("func NewCustomClient(cc grpc.ClientConnInterface) CustomClient {\n")
	b.WriteString("\treturn &customClient{inner: pb.NewCustomServiceClient(cc)}\n")
	b.WriteString("}\n\n")

	for _, q := range g.Queries {
		fmt.Fprintf(&b, `func (c *customClient) %s(ctx context.Context, req *pb.%sRequest, opts ...grpc.CallOption) (*pb.%sResponse, error) {
	return c.inner.%s(ctx, req, opts...)
}

`, q.Name, q.Name, q.Name, q.Name)
	}
	for _, p := range g.Procedures {
		fmt.Fprintf(&b, `func (c *customClient) %s(ctx context.Context, req *pb.%sRequest, opts ...grpc.CallOption) (*pb.%sResponse, error) {
	return c.inner.%s(ctx, req, opts...)
}

`, p.Name, p.Name, p.Name, p.Name)
	}

	path := fmt.Sprintf("clients/go/client/%s/custom_client.go", goNamespace(ns))
	return GoFile{Path: path, Content: b.String()}, nil
}

// ---- helpers ----

// normalizeSQLParams rewrites a query's raw SQL body, replacing every
// `$name` with `$N` where N is the 1-based ordinal of `name` in
// inputs. Returns the rewritten SQL plus the input names in
// placeholder order so the caller can emit `req.GetX()` calls in the
// right sequence.
//
// The same scan rules as sqlvalidate.normalizeNamedParams apply:
// single-quoted strings, double-quoted identifiers, and dollar-quoted
// blocks are passed through unchanged. PG positional `$<digit>`
// placeholders are also passed through, so an engineer who mixes
// named and positional shapes gets the named ones rewritten without
// disturbing the positional ones — though mixing is discouraged.
func normalizeSQLParams(sql string, inputs []dsl.QueryParam) (string, []string, error) {
	ordinal := make(map[string]int, len(inputs))
	for i, p := range inputs {
		ordinal[p.Name] = i + 1
	}
	return normalizeSQLParamsCore(sql, ordinal, inputs)
}

func normalizeSQLParamsRaw(sql string, ordinal map[string]int) (string, []string, error) {
	// Build a synthetic inputs slice from the ordinal map's keys; the
	// scan needs the input list only for resolving unknown args.
	return normalizeSQLParamsCore(sql, ordinal, nil)
}

func normalizeSQLParamsCore(sql string, ordinal map[string]int, inputs []dsl.QueryParam) (string, []string, error) {
	var b strings.Builder
	b.Grow(len(sql))
	// `order` lists each unique referenced input name once, in first-
	// reference order. `localPos` maps an input name to its 1-based
	// position within `order`. The emitted SQL uses those local
	// positions ($1..$M where M = len(order)) so the caller can bind
	// args by iterating `order` — one arg per unique input, regardless
	// of how many times the input appears in the SQL text.
	var order []string
	localPos := make(map[string]int)
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch c {
		case '\'':
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					b.WriteByte('\'')
					if i+1 < len(sql) && sql[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '"':
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '"' {
					b.WriteByte('"')
					if i+1 < len(sql) && sql[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '$':
			if i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				// Pre-existing PG positional placeholder; pass through.
				b.WriteByte('$')
				continue
			}
			if i+1 < len(sql) && isLetterOrUnderscoreCG(sql[i+1]) {
				j := i + 1
				for j < len(sql) && isIdentRuneCG(sql[j]) {
					j++
				}
				name := sql[i+1 : j]
				if _, ok := ordinal[name]; !ok {
					return "", nil, fmt.Errorf("$%s is not a declared input parameter", name)
				}
				pos, seen := localPos[name]
				if !seen {
					order = append(order, name)
					pos = len(order)
					localPos[name] = pos
				}
				fmt.Fprintf(&b, "$%d", pos)
				i = j - 1
				continue
			}
			b.WriteByte('$')
		default:
			b.WriteByte(c)
		}
	}
	_ = inputs // present on the signature so callers can pin a typed list when convenient
	return b.String(), order, nil
}

func isLetterOrUnderscoreCG(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isIdentRuneCG(c byte) bool {
	return isLetterOrUnderscoreCG(c) || (c >= '0' && c <= '9')
}
