package sqlvalidate

import (
	"errors"
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// safeBackfillFunctions is the closed set of function names a backfill
// expression may call. Anything outside this set is rejected because the
// expression runs server-side at apply time with the atlantis server's
// PG privileges — `nextval()` would mutate sequence state, `current_setting()`
// would leak runtime config, `pg_read_file()` would read arbitrary files
// from the data directory. The whitelist is intentionally tiny; operators
// who need more lobby for additions one function at a time.
var safeBackfillFunctions = map[string]bool{
	"coalesce":    true,
	"nullif":      true,
	"lower":       true,
	"upper":       true,
	"length":      true,
	"char_length": true,
	"trim":        true,
	"ltrim":       true,
	"rtrim":       true,
	"abs":         true,
	"concat":      true,
	"concat_ws":   true,
	"substring":   true,
	"substr":      true,
	"replace":     true,
	"left":        true,
	"right":       true,
	"to_char":     true,
}

// ValidateBackfillExpression parses the user-supplied SQL expression
// behind a field's `backfill "<expr>"` modifier and rejects it if any of:
//
//   - it does not parse as a valid scalar SQL expression
//   - it contains a subquery (SubLink) — would let the expression read
//     arbitrary tables, including ones the operator doesn't intend
//   - it calls a function outside safeBackfillFunctions — would let
//     volatile / side-effect-producing functions slip into the apply path
//   - it references its own field (selfFieldName) — pointless and
//     misleading
//   - it references a column not present in allowedCols — typos or
//     stale references that would silently break at apply time
//
// allowedCols should be the column set of the entity carrying the field,
// minus any columns being added in the same plan (those aren't populated
// yet when the backfill runs).
//
// The parser sees `SELECT (<expr>)` so the expression position matches
// its real apply-time context (the RHS of `SET <col> = <expr>` in the
// chunked UPDATE). Wrapping in SELECT not UPDATE keeps the validator
// independent of schema layout — it doesn't need a real table to parse.
func ValidateBackfillExpression(expr string, allowedCols map[string]bool, selfFieldName string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("backfill expression is empty")
	}
	src := fmt.Sprintf("SELECT (%s)", expr)
	tree, err := pg.Parse(src)
	if err != nil {
		return fmt.Errorf("backfill expression parse failed: %w", err)
	}
	if len(tree.Stmts) != 1 {
		return fmt.Errorf("backfill expression must be a single scalar; got %d statements", len(tree.Stmts))
	}

	var errs []string
	var walk func(n *pg.Node)
	walk = func(n *pg.Node) {
		if n == nil {
			return
		}
		switch x := n.GetNode().(type) {
		case *pg.Node_SubLink:
			errs = append(errs, "backfill expression cannot contain subqueries")
			// Don't recurse into the subquery — one error is enough.
			return
		case *pg.Node_FuncCall:
			name := funcCallName(x.FuncCall)
			if !safeBackfillFunctions[strings.ToLower(name)] {
				errs = append(errs, fmt.Sprintf("backfill expression cannot call %q (not in safe-function whitelist)", name))
			}
			for _, arg := range x.FuncCall.GetArgs() {
				walk(arg)
			}
		case *pg.Node_ColumnRef:
			name := columnRefName(x.ColumnRef)
			if name == "" {
				return
			}
			if selfFieldName != "" && name == selfFieldName {
				errs = append(errs, fmt.Sprintf("backfill expression cannot reference its own column %q", name))
				return
			}
			if !allowedCols[name] {
				errs = append(errs, fmt.Sprintf("backfill expression references unknown column %q", name))
			}
		case *pg.Node_SelectStmt:
			for _, t := range x.SelectStmt.TargetList {
				walk(t)
			}
		case *pg.Node_ResTarget:
			walk(x.ResTarget.Val)
		case *pg.Node_AExpr:
			walk(x.AExpr.Lexpr)
			walk(x.AExpr.Rexpr)
		case *pg.Node_BoolExpr:
			for _, a := range x.BoolExpr.GetArgs() {
				walk(a)
			}
		case *pg.Node_CaseExpr:
			for _, a := range x.CaseExpr.GetArgs() {
				walk(a)
			}
			walk(x.CaseExpr.Defresult)
			walk(x.CaseExpr.Arg)
		case *pg.Node_CaseWhen:
			walk(x.CaseWhen.Expr)
			walk(x.CaseWhen.Result)
		case *pg.Node_List:
			for _, item := range x.List.GetItems() {
				walk(item)
			}
		case *pg.Node_TypeCast:
			walk(x.TypeCast.Arg)
		case *pg.Node_CoalesceExpr:
			for _, a := range x.CoalesceExpr.GetArgs() {
				walk(a)
			}
		case *pg.Node_NullTest:
			walk(x.NullTest.Arg)
		case *pg.Node_AArrayExpr:
			for _, el := range x.AArrayExpr.GetElements() {
				walk(el)
			}
		case *pg.Node_RowExpr:
			for _, a := range x.RowExpr.GetArgs() {
				walk(a)
			}
		}
	}
	for _, rawStmt := range tree.Stmts {
		walk(rawStmt.GetStmt())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func funcCallName(fc *pg.FuncCall) string {
	if fc == nil || len(fc.Funcname) == 0 {
		return ""
	}
	last := fc.Funcname[len(fc.Funcname)-1]
	if s := last.GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}

func columnRefName(cr *pg.ColumnRef) string {
	if cr == nil || len(cr.Fields) == 0 {
		return ""
	}
	last := cr.Fields[len(cr.Fields)-1]
	if s := last.GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}
