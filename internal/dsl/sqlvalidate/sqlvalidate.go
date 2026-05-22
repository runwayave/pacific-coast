// Package sqlvalidate is the pg_query_go-backed validator for the raw
// SQL embedded in `query` and `procedure` declarations. It runs at
// tidectl plan time (or any caller-side build that wants deep validation),
// not on every codegen pass — pg_query_go is a CGO dep and we keep it
// off the hot codegen path. The dep-free pre-checks in
// internal/dsl/ir.go cover the most common failures ($arg references,
// touches() resolution, typed-step column existence); this package
// adds the deeper, PG-aware checks.
//
// Checks performed:
//
//   - SQL parses with the same grammar Postgres uses (pg_query is a
//     fork of the PG parser).
//   - Every table referenced in a FROM / JOIN / UPDATE / DELETE /
//     INSERT clause resolves to an entity in the merged IR. Caller
//     queries cannot reference tables that do not exist in the
//     schema — typos and stale references surface at plan time
//     rather than at first execution.
//   - Statement shape matches the declaration form: queries must use
//     a SELECT statement (or a CTE that resolves to one); procedure
//     raw-SQL steps may use SELECT, INSERT, UPDATE, or DELETE, but
//     not DDL (CREATE, ALTER, DROP, GRANT, etc.) — DDL must flow
//     through the autogen migration path so atlantis retains
//     authority over the schema.
//
// Future checks (deferred until a callsite demands them):
//
//   - Column-existence validation. Walking pg_query's expression tree
//     to extract every ColumnRef and binding it back to entity fields
//     means handling aliases, sub-selects, lateral joins, and column
//     ambiguity — non-trivial. The IR-level `touches()` check + the
//     test pass at tidectl plan time catches most regressions in
//     practice. When a callsite needs tighter column validation, we
//     add it here.
//   - Cost estimation via EXPLAIN. Requires a live PG connection and
//     belongs in tidectl plan rather than here.
package sqlvalidate

import (
	"errors"
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Mode toggles between the validation flavors for the two raw-SQL
// contexts. Queries permit only SELECT shapes; procedure raw steps
// additionally permit DML.
type Mode int

const (
	// ModeQuery is the validation for `query { sql touches(...) { ... } }`
	// blocks. Statement must be SELECT (or CTE/WITH wrapping a SELECT).
	ModeQuery Mode = iota
	// ModeProcedureStep is the validation for `procedure { steps { sql
	// touches(...) { ... } } }` blocks. DML allowed; DDL still rejected.
	ModeProcedureStep
)

// ValidateCustomQuery runs the deep validator over a single CustomQuery.
// Errors are aggregated rather than fast-failing so plan-time output
// surfaces every problem in one pass.
func ValidateCustomQuery(ir *dsl.IR, q *dsl.CustomQuery) error {
	var errs []error
	tableSet := buildTableSet(ir)
	if e := validateBlock(q.SQL, ModeQuery, tableSet, fmt.Sprintf("query %s", q.ID()), q.Touches); e != nil {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// ValidateCustomProcedure runs the deep validator over every raw step
// inside a procedure. Typed steps are validated structurally at IR
// lowering — they don't reach pg_query_go because their SQL is
// generated from existing entity templates.
func ValidateCustomProcedure(ir *dsl.IR, p *dsl.CustomProcedure) error {
	var errs []error
	tableSet := buildTableSet(ir)
	for i, step := range p.Steps {
		if step.Raw == nil {
			continue
		}
		ctx := fmt.Sprintf("procedure %s step %d", p.ID(), i+1)
		if e := validateBlock(step.Raw.SQL, ModeProcedureStep, tableSet, ctx, step.Raw.Touches); e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// validateBlock runs the parser pass + statement-kind + table-reference
// checks against one raw SQL body.
//
// The body uses DSL-style named placeholders (`$consumer_id`), but
// pg_query_go's parser requires PG positional shape (`$1`, `$2`, ...).
// We normalize before parsing so pg_query sees syntactically valid
// PG. The IR layer already validated that every $name resolves to a
// declared input, so this rewrite is purely syntactic — it does not
// change semantics or table/column references.
func validateBlock(sql string, mode Mode, tables map[string]string, context string, touches []string) error {
	tree, err := pg.Parse(normalizeNamedParams(sql))
	if err != nil {
		return fmt.Errorf("%s: SQL parse failed: %w", context, err)
	}
	if len(tree.Stmts) == 0 {
		return fmt.Errorf("%s: SQL body is empty", context)
	}

	var errs []error
	var refTables []string
	for i, rawStmt := range tree.Stmts {
		stmt := rawStmt.GetStmt()
		if err := checkStatementKind(stmt, mode); err != nil {
			errs = append(errs, fmt.Errorf("%s: statement %d: %w", context, i+1, err))
			continue
		}
		ctes := collectCTENames(stmt)
		refs := collectTableRefs(stmt)
		for _, ref := range refs {
			// CTE names look like ordinary table references in the
			// parse tree but resolve against the WITH clause, not the
			// schema. Skip them so they don't trip the unknown-table
			// check.
			if _, isCTE := ctes[strings.ToLower(ref)]; isCTE {
				continue
			}
			canonical, ok := resolveTable(tables, ref)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: SQL references unknown table %q (no entity maps to this name)", context, ref))
				continue
			}
			refTables = append(refTables, canonical)
		}
	}

	// touches() must be a superset of every entity actually referenced
	// in the SQL. The cache layer derives generation bumps from
	// touches; if the SQL reads or writes an entity that isn't in
	// touches, that entity's bumps won't fire and cached results go
	// stale silently.
	if err := checkTouchesCoverage(touches, refTables, context); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// checkStatementKind enforces the allowed top-level statement shapes
// per validation mode. Anything not on the allow-list is rejected with
// a pointed error — including DDL, which atlantis routes through
// the autogen migration path, not the runtime query surface.
func checkStatementKind(stmt *pg.Node, mode Mode) error {
	switch n := stmt.GetNode().(type) {
	case *pg.Node_SelectStmt:
		return nil
	case *pg.Node_InsertStmt, *pg.Node_UpdateStmt, *pg.Node_DeleteStmt:
		if mode == ModeQuery {
			return fmt.Errorf("query{} bodies must be SELECT statements; got %s", statementName(n))
		}
		return nil
	default:
		return fmt.Errorf("disallowed statement kind %T (DDL and other non-DML shapes go through tidectl migrations, not raw SQL)", n)
	}
}

// statementName returns a short human-readable name for a statement
// node. Used in error messages.
func statementName(n any) string {
	switch n.(type) {
	case *pg.Node_SelectStmt:
		return "SELECT"
	case *pg.Node_InsertStmt:
		return "INSERT"
	case *pg.Node_UpdateStmt:
		return "UPDATE"
	case *pg.Node_DeleteStmt:
		return "DELETE"
	}
	return fmt.Sprintf("%T", n)
}

// collectTableRefs returns every table name referenced by a parsed
// statement. RangeVars appear in FROM clauses, JOINs, UPDATE targets,
// DELETE FROM, INSERT INTO, and CTE definitions. The walk is recursive
// across nested SELECTs (sub-selects, lateral joins, CTE bodies).
func collectTableRefs(stmt *pg.Node) []string {
	var out []string
	var walk func(n *pg.Node)
	walk = func(n *pg.Node) {
		if n == nil {
			return
		}
		switch x := n.GetNode().(type) {
		case *pg.Node_RangeVar:
			if x.RangeVar != nil {
				out = append(out, normalizeTableRef(x.RangeVar))
			}
		case *pg.Node_SelectStmt:
			s := x.SelectStmt
			for _, f := range s.FromClause {
				walk(f)
			}
			for _, t := range s.TargetList {
				walk(t)
			}
			for _, c := range s.WithClause.GetCtes() {
				walk(c)
			}
			if s.WhereClause != nil {
				walk(s.WhereClause)
			}
			if s.HavingClause != nil {
				walk(s.HavingClause)
			}
			// Set ops: UNION / INTERSECT / EXCEPT.
			walk(rangeNode(s.Larg))
			walk(rangeNode(s.Rarg))
		case *pg.Node_InsertStmt:
			if x.InsertStmt != nil {
				if x.InsertStmt.Relation != nil {
					out = append(out, normalizeTableRef(x.InsertStmt.Relation))
				}
				if x.InsertStmt.SelectStmt != nil {
					walk(x.InsertStmt.SelectStmt)
				}
				if x.InsertStmt.WithClause != nil {
					for _, c := range x.InsertStmt.WithClause.GetCtes() {
						walk(c)
					}
				}
			}
		case *pg.Node_UpdateStmt:
			if x.UpdateStmt != nil {
				if x.UpdateStmt.Relation != nil {
					out = append(out, normalizeTableRef(x.UpdateStmt.Relation))
				}
				for _, f := range x.UpdateStmt.FromClause {
					walk(f)
				}
				if x.UpdateStmt.WhereClause != nil {
					walk(x.UpdateStmt.WhereClause)
				}
			}
		case *pg.Node_DeleteStmt:
			if x.DeleteStmt != nil {
				if x.DeleteStmt.Relation != nil {
					out = append(out, normalizeTableRef(x.DeleteStmt.Relation))
				}
				for _, f := range x.DeleteStmt.UsingClause {
					walk(f)
				}
				if x.DeleteStmt.WhereClause != nil {
					walk(x.DeleteStmt.WhereClause)
				}
			}
		case *pg.Node_JoinExpr:
			walk(x.JoinExpr.Larg)
			walk(x.JoinExpr.Rarg)
		case *pg.Node_RangeSubselect:
			walk(x.RangeSubselect.Subquery)
		case *pg.Node_CommonTableExpr:
			walk(x.CommonTableExpr.Ctequery)
		case *pg.Node_SubLink:
			walk(x.SubLink.Subselect)
		case *pg.Node_ResTarget:
			walk(x.ResTarget.Val)
		}
	}
	walk(stmt)
	return out
}

// rangeNode wraps an arbitrary SelectStmt pointer back into a Node so
// the walk function can recurse via the common entry point. Returns
// nil for nil input.
func rangeNode(s *pg.SelectStmt) *pg.Node {
	if s == nil {
		return nil
	}
	return &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: s}}
}

// collectCTENames walks a parsed statement and returns the set of
// names defined by WITH clauses anywhere in the tree (including
// nested SELECTs and DML statements). Names are lower-cased because
// PG identifiers are case-insensitive by default. The resulting set
// is consulted before reporting an "unknown table" error so a CTE
// alias used downstream in the same statement doesn't surface as a
// schema gap.
func collectCTENames(stmt *pg.Node) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(n *pg.Node)
	walk = func(n *pg.Node) {
		if n == nil {
			return
		}
		switch x := n.GetNode().(type) {
		case *pg.Node_SelectStmt:
			s := x.SelectStmt
			for _, c := range s.WithClause.GetCtes() {
				if cte := c.GetCommonTableExpr(); cte != nil {
					out[strings.ToLower(cte.Ctename)] = struct{}{}
					walk(cte.Ctequery)
				}
			}
			for _, f := range s.FromClause {
				walk(f)
			}
			walk(rangeNode(s.Larg))
			walk(rangeNode(s.Rarg))
		case *pg.Node_InsertStmt:
			if x.InsertStmt != nil && x.InsertStmt.WithClause != nil {
				for _, c := range x.InsertStmt.WithClause.GetCtes() {
					if cte := c.GetCommonTableExpr(); cte != nil {
						out[strings.ToLower(cte.Ctename)] = struct{}{}
						walk(cte.Ctequery)
					}
				}
			}
			walk(x.InsertStmt.GetSelectStmt())
		case *pg.Node_UpdateStmt:
			if x.UpdateStmt != nil && x.UpdateStmt.WithClause != nil {
				for _, c := range x.UpdateStmt.WithClause.GetCtes() {
					if cte := c.GetCommonTableExpr(); cte != nil {
						out[strings.ToLower(cte.Ctename)] = struct{}{}
						walk(cte.Ctequery)
					}
				}
			}
		case *pg.Node_DeleteStmt:
			if x.DeleteStmt != nil && x.DeleteStmt.WithClause != nil {
				for _, c := range x.DeleteStmt.WithClause.GetCtes() {
					if cte := c.GetCommonTableExpr(); cte != nil {
						out[strings.ToLower(cte.Ctename)] = struct{}{}
						walk(cte.Ctequery)
					}
				}
			}
		case *pg.Node_RangeSubselect:
			walk(x.RangeSubselect.Subquery)
		case *pg.Node_JoinExpr:
			walk(x.JoinExpr.Larg)
			walk(x.JoinExpr.Rarg)
		}
	}
	walk(stmt)
	return out
}

// normalizeTableRef produces a canonical "schema.table" string for a
// RangeVar reference. Unschema-qualified references default to the
// SQL's notional namespace; the resolver layer maps both forms to
// entity ids.
func normalizeTableRef(rv *pg.RangeVar) string {
	if rv.Schemaname != "" {
		return rv.Schemaname + "." + rv.Relname
	}
	return rv.Relname
}

// resolveTable maps a SQL table reference back to an entity id. The
// mapping mirrors the table-name convention in internal/codegen/sql.go:
// `tableName(e) = e.Namespace + "_" + snake_case(e.Name)`, inside the
// `atlantis` schema. References can therefore appear as any of:
//
//   - `atlantis.consumer_account` — fully qualified
//   - `consumer_account` — unqualified, schema defaults to search_path
//   - other schemas — rejected (only atlantis is owned by the codegen)
func resolveTable(tables map[string]string, ref string) (string, bool) {
	ref = strings.ToLower(ref)
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		schema, table := ref[:i], ref[i+1:]
		if schema != "atlantis" {
			return "", false
		}
		id, ok := tables[table]
		return id, ok
	}
	id, ok := tables[ref]
	return id, ok
}

// buildTableSet computes the SQL-table-name -> entity-id lookup once
// per validation pass. Lower-cases the key so PG's case-insensitive
// identifier matching works transparently.
func buildTableSet(ir *dsl.IR) map[string]string {
	out := make(map[string]string, len(ir.Entities))
	for i := range ir.Entities {
		e := &ir.Entities[i]
		name := strings.ToLower(e.Namespace + "_" + snakeCase(e.Name))
		out[name] = e.ID()
	}
	return out
}

// checkTouchesCoverage compares the entities the SQL actually
// references against the touches() declaration. If the SQL touches an
// entity the engineer didn't list, that entity's cache won't be
// bumped on write — the validator catches this before the misconfig
// reaches production.
//
// The reverse (touches lists an entity the SQL never references) is
// caught at IR lowering time as "input never referenced in SQL"-style
// guards on the touches list; this function only fires on
// undeclared references.
func checkTouchesCoverage(declared []string, referenced []string, context string) error {
	if len(referenced) == 0 {
		return nil
	}
	declSet := make(map[string]bool, len(declared))
	for _, d := range declared {
		declSet[d] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, r := range referenced {
		if seen[r] {
			continue
		}
		seen[r] = true
		if !declSet[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: SQL references %s but touches() does not declare them; cache invalidation would be silently stale", context, strings.Join(missing, ", "))
	}
	return nil
}

// normalizeNamedParams replaces DSL-style `$ident` placeholders with PG
// positional `$1` so pg_query_go's parser accepts the body. The
// validator only cares about table / column / statement-kind
// resolution; the positional index doesn't matter for that. The
// rewrite is intentionally crude — it does not track which name maps
// to which positional index, because the IR layer already validated
// argument names against the declared input list, and the codegen
// layer does the real index assignment at SQL emit time.
//
// Skipped contexts:
//
//   - Inside single-quoted SQL strings (`'...'`). `'$foo'` is a literal
//     value, not a parameter. Same for embedded `”` escapes.
//   - Inside double-quoted SQL identifiers (`"...$foo..."`). Real PG
//     identifiers can contain `$` so we leave them untouched.
//   - Already-numeric `$<digit>` shapes pass through unchanged.
//
// Dollar-quoted strings (`$tag$ ... $tag$`) are not specially handled;
// the brace-counter in lexer.captureRawSQLBody also doesn't recognize
// them. Caller workloads in the audit don't use dollar quoting; if a
// real callsite hits this we'll add handling.
func normalizeNamedParams(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch c {
		case '\'':
			// Skip a single-quoted string verbatim, handling `''` as
			// the in-string escape for a literal quote.
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
			// Skip a double-quoted identifier verbatim, with `""` as
			// the in-string escape.
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
			// `$<digit>...` is PG positional; pass through.
			if i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				b.WriteByte('$')
				continue
			}
			// `$<ident>` is the DSL-named form; rewrite to `$1`.
			if i+1 < len(sql) && (isLetterOrUnderscore(sql[i+1])) {
				// Consume the ident.
				j := i + 1
				for j < len(sql) && isIdentChar(sql[j]) {
					j++
				}
				b.WriteString("$1")
				i = j - 1
				continue
			}
			b.WriteByte('$')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isLetterOrUnderscore(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isIdentChar(c byte) bool {
	return isLetterOrUnderscore(c) || (c >= '0' && c <= '9')
}

// snakeCase mirrors the entity name → SQL table conversion used by the
// codegen layer. Kept in lockstep with internal/codegen/sql.go's
// snakeCase — the entity SavedOutfit lowers to `saved_outfit`, so a
// query referencing `consumer_saved_outfit` resolves to consumer.SavedOutfit.
func snakeCase(s string) string {
	var out []rune
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			out = append(out, '_')
		}
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		out = append(out, r)
	}
	return string(out)
}
