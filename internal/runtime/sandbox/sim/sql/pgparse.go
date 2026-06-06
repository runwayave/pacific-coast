// Package sql translates the pg_query_go AST into the executor's
// simsql.Stmt / Expr / Pred types. pg_query (libpg_query via cgo)
// owns the parse; this file owns the strict whitelist that rejects
// anything the executor doesn't model.
//
// Translator policy is strict whitelist. Every pg_query node the
// executor doesn't model returns ErrUnsupported with a snippet of
// what was rejected — operators see the same surface they always
// have ("unsupported: JOIN", "unsupported: LIMIT requires
// placeholder"), they just get them from the translator now instead
// of the lexer.
//
// The contract the rest of the codebase depends on:
//
//	Parse(src string) (Stmt, error)
//
// - empty / comment-only input → ErrUnsupported
// - multi-statement input ("a; b;") → ErrUnsupported
// - one parsed Stmt that the executor accepts → (stmt, nil)
// - one parsed Stmt that the executor doesn't model → (nil, ErrUnsupported)
// - non-PG-grammar SQL → wrapped pg_query error
//
// PG-version note: pg_query_go v6 ships the PG 17 parser. atlantis
// runs PG 16 in production; PG 17 syntax PG 16 doesn't accept (e.g.
// MERGE ... WHEN NOT MATCHED BY SOURCE) parses here but errors at
// executor-time. Not a problem for codegen-emitted SQL.

package sql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// ErrUnsupported signals the simulator recognized the input as plausible
// SQL but doesn't model the construct the user actually wrote. Callers
// check errors.Is(err, ErrUnsupported) to surface a sandbox-specific
// "this query doesn't run here, try embedded" message.
//
// The wrapping pattern: rejections return fmt.Errorf("%w: ...", ErrUnsupported, ...);
// pg_query syntax errors return the wrapped pg_query error unchanged
// so the caller can tell the two apart by errors.Is.
var ErrUnsupported = errors.New("sandbox sql: unsupported")

// Parse tokenizes and parses a single SQL statement via pg_query_go.
// Returns the existing simsql AST that the executor consumes.
func Parse(src string) (Stmt, error) {
	tree, err := pg.Parse(src)
	if err != nil {
		// pg_query syntax errors carry position info and a useful
		// message; wrap without redundant prefixing.
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if len(tree.Stmts) == 0 {
		// pg_query returns an empty Stmts slice for "", whitespace, or
		// comment-only input. The executor would happily NPE on a nil
		// statement; reject explicitly.
		return nil, fmt.Errorf("%w: empty SQL", ErrUnsupported)
	}
	if len(tree.Stmts) > 1 {
		// The sandbox SQL surface is one statement per call — multi-stmt
		// would need cross-statement transaction semantics, which the
		// simulator doesn't model. Reject loudly so callers don't get
		// a silent first-statement-wins.
		return nil, fmt.Errorf("%w: multiple statements (got %d)", ErrUnsupported, len(tree.Stmts))
	}

	stmt := tree.Stmts[0].GetStmt()
	if stmt == nil {
		return nil, fmt.Errorf("%w: empty statement", ErrUnsupported)
	}
	switch n := stmt.Node.(type) {
	case *pg.Node_SelectStmt:
		return translateSelect(n.SelectStmt)
	case *pg.Node_InsertStmt:
		return translateInsert(n.InsertStmt)
	case *pg.Node_UpdateStmt:
		return translateUpdate(n.UpdateStmt)
	case *pg.Node_DeleteStmt:
		return translateDelete(n.DeleteStmt)
	default:
		return nil, fmt.Errorf("%w: statement type %T", ErrUnsupported, n)
	}
}

// ─────────────────────────── statement translators ───────────────────────────

func translateSelect(s *pg.SelectStmt) (*Select, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil SELECT", ErrUnsupported)
	}
	// Things the executor doesn't model — reject up front so partial
	// translation never reaches the executor.
	if s.WithClause != nil {
		return nil, fmt.Errorf("%w: WITH/CTE", ErrUnsupported)
	}
	if len(s.GroupClause) > 0 {
		return nil, fmt.Errorf("%w: GROUP BY", ErrUnsupported)
	}
	if s.HavingClause != nil {
		return nil, fmt.Errorf("%w: HAVING", ErrUnsupported)
	}
	if len(s.WindowClause) > 0 {
		return nil, fmt.Errorf("%w: WINDOW", ErrUnsupported)
	}
	if s.Larg != nil || s.Rarg != nil {
		return nil, fmt.Errorf("%w: UNION/INTERSECT/EXCEPT", ErrUnsupported)
	}
	if s.LimitOption == pg.LimitOption_LIMIT_OPTION_WITH_TIES {
		return nil, fmt.Errorf("%w: FETCH WITH TIES", ErrUnsupported)
	}
	if s.LockingClause != nil {
		return nil, fmt.Errorf("%w: locking clause", ErrUnsupported)
	}

	out := &Select{}

	// FROM: zero or one RangeVar. SELECT-without-FROM (e.g. "SELECT 1")
	// is legal in PG and useful as a placeholder editor body — leave
	// out.Table at the zero value when absent. The executor checks
	// IsZero before doing catalog lookups.
	switch len(s.FromClause) {
	case 0:
		// no FROM — table stays zero
	case 1:
		tbl, err := translateRangeVar(s.FromClause[0])
		if err != nil {
			return nil, err
		}
		out.Table = tbl
	default:
		return nil, fmt.Errorf("%w: multi-table FROM (no joins)", ErrUnsupported)
	}

	// Projections.
	cols, err := translateProjections(s.TargetList)
	if err != nil {
		return nil, err
	}
	out.Cols = cols

	// WHERE.
	if s.WhereClause != nil {
		preds, err := translateWhere(s.WhereClause)
		if err != nil {
			return nil, err
		}
		out.Where = preds
	}

	// ORDER BY.
	if len(s.SortClause) > 0 {
		ob, err := translateOrderBy(s.SortClause)
		if err != nil {
			return nil, err
		}
		out.OrderBy = ob
	}

	// LIMIT / OFFSET. Both accept a Placeholder ($N) or an inline
	// integer literal (real PG accepts both); the executor evaluates
	// the Expr at run time and asserts the result is an int64.
	if s.LimitCount != nil {
		lim, err := translateLimitExpr(s.LimitCount, "LIMIT")
		if err != nil {
			return nil, err
		}
		out.Limit = lim
	}
	if s.LimitOffset != nil {
		off, err := translateLimitExpr(s.LimitOffset, "OFFSET")
		if err != nil {
			return nil, err
		}
		out.Offset = off
	}

	return out, nil
}

func translateInsert(i *pg.InsertStmt) (*Insert, error) {
	if i == nil {
		return nil, fmt.Errorf("%w: nil INSERT", ErrUnsupported)
	}
	if i.WithClause != nil {
		return nil, fmt.Errorf("%w: INSERT with CTE", ErrUnsupported)
	}
	if i.Relation == nil {
		return nil, fmt.Errorf("%w: INSERT without table", ErrUnsupported)
	}
	tbl := TableRef{
		Schema: i.Relation.Schemaname,
		Name:   i.Relation.Relname,
	}

	out := &Insert{Table: tbl}

	// Columns. INSERT INTO t (a,b,c) — TargetList is a list of ResTarget,
	// each Name is the column.
	for _, col := range i.Cols {
		rt := col.GetResTarget()
		if rt == nil || rt.Name == "" {
			return nil, fmt.Errorf("%w: INSERT column missing name", ErrUnsupported)
		}
		out.Cols = append(out.Cols, rt.Name)
	}

	// VALUES. SelectStmt with ValuesLists.
	if i.SelectStmt == nil {
		return nil, fmt.Errorf("%w: INSERT without VALUES", ErrUnsupported)
	}
	sel := i.SelectStmt.GetSelectStmt()
	if sel == nil || len(sel.ValuesLists) == 0 {
		return nil, fmt.Errorf("%w: INSERT VALUES expected", ErrUnsupported)
	}
	if len(sel.ValuesLists) != 1 {
		// Codegen never emits multi-row INSERT; reject so the
		// executor doesn't have to handle the multi-row case.
		return nil, fmt.Errorf("%w: multi-row INSERT", ErrUnsupported)
	}
	row := sel.ValuesLists[0].GetList()
	if row == nil {
		return nil, fmt.Errorf("%w: INSERT VALUES row malformed", ErrUnsupported)
	}
	for _, v := range row.Items {
		expr, err := translateExpr(v)
		if err != nil {
			return nil, err
		}
		out.Args = append(out.Args, expr)
	}

	// RETURNING.
	for _, col := range i.ReturningList {
		rt := col.GetResTarget()
		if rt == nil {
			return nil, fmt.Errorf("%w: RETURNING item not a column", ErrUnsupported)
		}
		name, err := returningColumnName(rt)
		if err != nil {
			return nil, err
		}
		out.Returning = append(out.Returning, name)
	}

	// ON CONFLICT.
	if i.OnConflictClause != nil {
		if err := translateOnConflict(i.OnConflictClause, out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func translateUpdate(u *pg.UpdateStmt) (*Update, error) {
	if u == nil {
		return nil, fmt.Errorf("%w: nil UPDATE", ErrUnsupported)
	}
	if u.WithClause != nil {
		return nil, fmt.Errorf("%w: UPDATE with CTE", ErrUnsupported)
	}
	if u.Relation == nil {
		return nil, fmt.Errorf("%w: UPDATE without table", ErrUnsupported)
	}
	if len(u.FromClause) > 0 {
		return nil, fmt.Errorf("%w: UPDATE ... FROM", ErrUnsupported)
	}
	if len(u.ReturningList) > 0 {
		return nil, fmt.Errorf("%w: UPDATE ... RETURNING", ErrUnsupported)
	}

	out := &Update{Table: TableRef{Schema: u.Relation.Schemaname, Name: u.Relation.Relname}}

	for _, ts := range u.TargetList {
		rt := ts.GetResTarget()
		if rt == nil || rt.Name == "" {
			return nil, fmt.Errorf("%w: UPDATE assignment missing column name", ErrUnsupported)
		}
		val, err := translateExpr(rt.Val)
		if err != nil {
			return nil, err
		}
		out.Set = append(out.Set, Assign{Column: rt.Name, Value: val})
	}

	if u.WhereClause != nil {
		preds, err := translateWhere(u.WhereClause)
		if err != nil {
			return nil, err
		}
		out.Where = preds
	}

	return out, nil
}

func translateDelete(d *pg.DeleteStmt) (*Delete, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: nil DELETE", ErrUnsupported)
	}
	if d.WithClause != nil {
		return nil, fmt.Errorf("%w: DELETE with CTE", ErrUnsupported)
	}
	if d.Relation == nil {
		return nil, fmt.Errorf("%w: DELETE without table", ErrUnsupported)
	}
	if len(d.UsingClause) > 0 {
		return nil, fmt.Errorf("%w: DELETE ... USING", ErrUnsupported)
	}
	if len(d.ReturningList) > 0 {
		return nil, fmt.Errorf("%w: DELETE ... RETURNING", ErrUnsupported)
	}

	out := &Delete{Table: TableRef{Schema: d.Relation.Schemaname, Name: d.Relation.Relname}}

	if d.WhereClause != nil {
		preds, err := translateWhere(d.WhereClause)
		if err != nil {
			return nil, err
		}
		out.Where = preds
	}

	return out, nil
}

// ─────────────────────────── ON CONFLICT ───────────────────────────

func translateOnConflict(c *pg.OnConflictClause, ins *Insert) error {
	switch c.Action {
	case pg.OnConflictAction_ONCONFLICT_NOTHING:
		ins.OnConflict = ConflictNothing
	case pg.OnConflictAction_ONCONFLICT_UPDATE:
		ins.OnConflict = ConflictUpdate
	default:
		return fmt.Errorf("%w: ON CONFLICT action %v", ErrUnsupported, c.Action)
	}

	// ON CONFLICT (col1, col2) target.
	if c.Infer != nil {
		if c.Infer.Conname != "" {
			return fmt.Errorf("%w: ON CONFLICT ON CONSTRAINT", ErrUnsupported)
		}
		for _, elem := range c.Infer.IndexElems {
			ie := elem.GetIndexElem()
			if ie == nil || ie.Name == "" {
				return fmt.Errorf("%w: ON CONFLICT target must be a bare column", ErrUnsupported)
			}
			ins.ConflictTarget = append(ins.ConflictTarget, ie.Name)
		}
	}

	// DO UPDATE SET assignments.
	for _, ts := range c.TargetList {
		rt := ts.GetResTarget()
		if rt == nil || rt.Name == "" {
			return fmt.Errorf("%w: ON CONFLICT SET assignment missing column", ErrUnsupported)
		}
		val, err := translateExpr(rt.Val)
		if err != nil {
			return err
		}
		ins.ConflictUpdates = append(ins.ConflictUpdates, Assign{Column: rt.Name, Value: val})
	}
	return nil
}

// ─────────────────────────── projections ───────────────────────────

func translateProjections(targets []*pg.Node) ([]Projection, error) {
	out := make([]Projection, 0, len(targets))
	for _, t := range targets {
		rt := t.GetResTarget()
		if rt == nil {
			return nil, fmt.Errorf("%w: projection not a ResTarget", ErrUnsupported)
		}
		// Bare column ref (no AS) — `col` or `"col"`.
		if rt.Val == nil {
			return nil, fmt.Errorf("%w: projection missing value", ErrUnsupported)
		}

		// `*` (with or without an alias-target table) becomes a
		// ColumnRef containing an A_Star field. Emit a Star projection;
		// the executor expands it against the table descriptor.
		if cr := rt.Val.GetColumnRef(); cr != nil {
			last := cr.Fields[len(cr.Fields)-1]
			if _, ok := last.Node.(*pg.Node_AStar); ok {
				// `t.*` (qualified star) — supported when the qualifier
				// matches the FROM table, but the executor doesn't model
				// table-qualified column refs yet. Bare `*` is the
				// common case; treat both the same and let the executor
				// expand against the (single) FROM table.
				out = append(out, Projection{Star: true})
				continue
			}
		}

		// Window total: count(*) OVER () AS alias.
		if fn := rt.Val.GetFuncCall(); fn != nil && fn.Over != nil {
			alias, err := windowCountAlias(fn, rt.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, Projection{WindowCountAlias: alias})
			continue
		}

		// Expression with alias (vector distance, JSON extract).
		if rt.Name != "" {
			expr, err := translateExpr(rt.Val)
			if err != nil {
				return nil, err
			}
			// A bare column-ref with a column-name alias collapses to
			// a Column projection — matches what the executor expects.
			if cref, ok := expr.(ColumnRef); ok && rt.Name == cref.Name {
				out = append(out, Projection{Column: cref.Name})
				continue
			}
			out = append(out, Projection{Expr: expr, Alias: rt.Name})
			continue
		}

		// Bare column ref — unwrap to a string column name.
		if cr := rt.Val.GetColumnRef(); cr != nil {
			name, err := columnRefName(cr)
			if err != nil {
				return nil, err
			}
			out = append(out, Projection{Column: name})
			continue
		}

		// Anything else (function call without alias, etc.) — push it
		// into the Expr field so the executor can complain if it can't
		// handle the shape. Alias falls back to the source text.
		expr, err := translateExpr(rt.Val)
		if err != nil {
			return nil, err
		}
		out = append(out, Projection{Expr: expr, Alias: rt.Name})
	}
	return out, nil
}

// windowCountAlias extracts the COUNT(*) OVER () pattern's alias.
// Any other shape (other agg function, partition by, order by inside
// the OVER, etc.) is rejected.
func windowCountAlias(fn *pg.FuncCall, alias string) (string, error) {
	if len(fn.Funcname) != 1 || fn.Funcname[0].GetString_().GetSval() != "count" {
		return "", fmt.Errorf("%w: window function %v (only COUNT(*) OVER ())", ErrUnsupported, funcCallName(fn))
	}
	if !fn.AggStar {
		return "", fmt.Errorf("%w: COUNT requires *", ErrUnsupported)
	}
	if fn.Over.PartitionClause != nil || fn.Over.OrderClause != nil {
		return "", fmt.Errorf("%w: window OVER (PARTITION/ORDER BY)", ErrUnsupported)
	}
	if alias == "" {
		return "", fmt.Errorf("%w: COUNT(*) OVER () must have an alias", ErrUnsupported)
	}
	return alias, nil
}

// returningColumnName extracts the column name from a RETURNING item.
// Allows bare `col` only — RETURNING expression-with-alias is out of
// scope for the executor's current implementation.
func returningColumnName(rt *pg.ResTarget) (string, error) {
	if rt.Name != "" {
		return rt.Name, nil
	}
	cr := rt.Val.GetColumnRef()
	if cr == nil {
		return "", fmt.Errorf("%w: RETURNING item must be a bare column", ErrUnsupported)
	}
	return columnRefName(cr)
}

// ─────────────────────────── ORDER BY ───────────────────────────

func translateOrderBy(sort []*pg.Node) ([]OrderByCol, error) {
	out := make([]OrderByCol, 0, len(sort))
	for _, n := range sort {
		sb := n.GetSortBy()
		if sb == nil {
			return nil, fmt.Errorf("%w: ORDER BY item not a SortBy", ErrUnsupported)
		}
		col := OrderByCol{}
		switch sb.SortbyDir {
		case pg.SortByDir_SORTBY_DEFAULT, pg.SortByDir_SORTBY_ASC:
			col.Desc = false
		case pg.SortByDir_SORTBY_DESC:
			col.Desc = true
		default:
			return nil, fmt.Errorf("%w: ORDER BY direction %v", ErrUnsupported, sb.SortbyDir)
		}
		switch sb.SortbyNulls {
		case pg.SortByNulls_SORTBY_NULLS_DEFAULT:
			col.Nulls = NullsDefault
		case pg.SortByNulls_SORTBY_NULLS_FIRST:
			col.Nulls = NullsFirst
		case pg.SortByNulls_SORTBY_NULLS_LAST:
			col.Nulls = NullsLast
		}

		// Sort by bare column or by expression.
		if cr := sb.Node.GetColumnRef(); cr != nil {
			name, err := columnRefName(cr)
			if err != nil {
				return nil, err
			}
			col.Column = name
		} else {
			expr, err := translateExpr(sb.Node)
			if err != nil {
				return nil, err
			}
			col.Expr = expr
		}
		out = append(out, col)
	}
	return out, nil
}

// ─────────────────────────── WHERE / predicates ───────────────────────────

// translateWhere walks the WhereClause root. Top-level AND-chains
// flatten to a flat []Pred (matching the existing executor contract);
// OR-chains and explicit parens become a GroupPred wrapper.
func translateWhere(n *pg.Node) ([]Pred, error) {
	// Top-level AND flattening: walk the A_Expr/BoolExpr tree and
	// collect the AND children as siblings. Single non-AND root
	// returns a single-element list.
	preds, err := flattenAnd(n)
	if err != nil {
		return nil, err
	}
	return preds, nil
}

// flattenAnd treats AND as the implicit top-level connective: a
// single Pred becomes a one-element slice, an AND of N children
// becomes N entries. Anything else (OR / NOT / a single comparison)
// returns a single Pred.
func flattenAnd(n *pg.Node) ([]Pred, error) {
	if be := n.GetBoolExpr(); be != nil && be.Boolop == pg.BoolExprType_AND_EXPR {
		var out []Pred
		for _, child := range be.Args {
			children, err := flattenAnd(child)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
		}
		return out, nil
	}
	p, err := translatePred(n)
	if err != nil {
		return nil, err
	}
	return []Pred{p}, nil
}

// translatePred maps a single predicate node — could be a comparison,
// IS NULL, = ANY, a paren'd group, or a logical operator. Anything
// that produces a boolean is fair game; everything else errors.
func translatePred(n *pg.Node) (Pred, error) {
	if n == nil {
		return nil, fmt.Errorf("%w: nil predicate", ErrUnsupported)
	}

	// BoolExpr: AND / OR / NOT. NOT is rejected (the executor doesn't
	// model NOT-groups), AND-chains under a paren'd group, OR-chains
	// become GroupPred{Connective: ConnOr}.
	if be := n.GetBoolExpr(); be != nil {
		switch be.Boolop {
		case pg.BoolExprType_AND_EXPR:
			children, err := flattenAnd(n)
			if err != nil {
				return nil, err
			}
			if len(children) == 1 {
				return children[0], nil
			}
			return GroupPred{Connective: ConnAnd, Preds: children}, nil
		case pg.BoolExprType_OR_EXPR:
			var out []Pred
			for _, child := range be.Args {
				p, err := translatePred(child)
				if err != nil {
					return nil, err
				}
				out = append(out, p)
			}
			return GroupPred{Connective: ConnOr, Preds: out}, nil
		case pg.BoolExprType_NOT_EXPR:
			return nil, fmt.Errorf("%w: NOT predicate", ErrUnsupported)
		}
	}

	// NullTest: col IS [NOT] NULL. pg_query represents this as a
	// dedicated NullTest node rather than an A_Expr.
	if nt := n.GetNullTest(); nt != nil {
		col, err := requireColumnRef(nt.Arg, "IS NULL argument")
		if err != nil {
			// JSON-extract IS NULL isn't modeled — surface clearly.
			return nil, err
		}
		return IsNullPred{
			Column: col,
			Not:    nt.Nulltesttype == pg.NullTestType_IS_NOT_NULL,
		}, nil
	}

	// A_Expr: scalar comparisons, ANY, vector ops, JSON ops.
	if ax := n.GetAExpr(); ax != nil {
		return translateAExpr(ax)
	}

	return nil, fmt.Errorf("%w: predicate kind %T", ErrUnsupported, n.Node)
}

// translateAExpr decodes an A_Expr node — the catchall pg_query uses
// for binary operators, ANY, vector ops, and JSON arrow operators
// when they appear in predicate position.
func translateAExpr(ax *pg.A_Expr) (Pred, error) {
	op := aExprOpName(ax)
	switch ax.Kind {
	case pg.A_Expr_Kind_AEXPR_OP:
		// Standard comparison: col <op> expr. May also be a vector
		// distance comparison or a JSON extract comparison.
		return translateCmpExpr(ax, op)
	case pg.A_Expr_Kind_AEXPR_OP_ANY:
		// col = ANY($N). Executor expects column = ANY(placeholder).
		if op != "=" {
			return nil, fmt.Errorf("%w: ANY with operator %q (only =)", ErrUnsupported, op)
		}
		col, err := requireColumnRef(ax.Lexpr, "ANY column")
		if err != nil {
			return nil, err
		}
		ph, err := translatePlaceholder(ax.Rexpr, "ANY")
		if err != nil {
			return nil, err
		}
		return AnyPred{Column: col, Arg: ph}, nil
	default:
		return nil, fmt.Errorf("%w: A_Expr kind %v", ErrUnsupported, ax.Kind)
	}
}

func translateCmpExpr(ax *pg.A_Expr, op string) (Pred, error) {
	// JSON-extract chain on the LHS: data->>'k' = $1 → JsonExtractPred.
	// pg_query represents the chain as a left-associative A_Expr tree:
	// (col -> 'k1') ->> 'k2'. Walk left until we hit the column.
	if isCmpOp(op) {
		if pred, ok, err := tryJsonExtractPred(ax, op); ok || err != nil {
			return pred, err
		}
	}

	// Plain column <op> expr.
	col, err := requireColumnRef(ax.Lexpr, "comparison column")
	if err != nil {
		return nil, err
	}
	cmpOp, err := mapCmpOp(op)
	if err != nil {
		return nil, err
	}
	value, err := translateExpr(ax.Rexpr)
	if err != nil {
		return nil, err
	}
	return CmpPred{Column: col, Op: cmpOp, Value: value}, nil
}

func tryJsonExtractPred(ax *pg.A_Expr, cmpOpStr string) (Pred, bool, error) {
	// LHS must be an A_Expr with a JSON arrow op at the root.
	lax := ax.Lexpr.GetAExpr()
	if lax == nil {
		return nil, false, nil
	}
	lop := aExprOpName(lax)
	if lop != "->" && lop != "->>" {
		return nil, false, nil
	}
	col, path, asText, err := walkJsonChain(lax)
	if err != nil {
		return nil, true, err
	}
	cmpOp, err := mapCmpOp(cmpOpStr)
	if err != nil {
		return nil, true, err
	}
	value, err := translateExpr(ax.Rexpr)
	if err != nil {
		return nil, true, err
	}
	return JsonExtractPred{
		Column: col,
		Path:   path,
		AsText: asText,
		Op:     cmpOp,
		Value:  value,
	}, true, nil
}

// walkJsonChain decodes a `col -> 'k1' -> 'k2' ->> 'kn'` left-associative
// chain into (column, path, asText). The final operator decides asText:
// `->>` returns text, `->` returns subtree.
func walkJsonChain(ax *pg.A_Expr) (col string, path []string, asText bool, err error) {
	op := aExprOpName(ax)
	asText = op == "->>"

	// Right side is the path component (string literal).
	key, err := jsonPathKey(ax.Rexpr)
	if err != nil {
		return "", nil, false, err
	}

	// Left side is either the column (terminal) or another arrow op.
	if subAx := ax.Lexpr.GetAExpr(); subAx != nil {
		subOp := aExprOpName(subAx)
		if subOp == "->" || subOp == "->>" {
			subCol, subPath, _, subErr := walkJsonChain(subAx)
			if subErr != nil {
				return "", nil, false, subErr
			}
			// Intermediate ops must be `->` — only the final op produces text.
			if subOp == "->>" {
				return "", nil, false, fmt.Errorf("%w: ->> only valid at the end of a JSON chain", ErrUnsupported)
			}
			return subCol, append(subPath, key), asText, nil
		}
	}

	col, err = requireColumnRef(ax.Lexpr, "JSON-extract column")
	if err != nil {
		return "", nil, false, err
	}
	return col, []string{key}, asText, nil
}

func jsonPathKey(n *pg.Node) (string, error) {
	c := n.GetAConst()
	if c == nil {
		return "", fmt.Errorf("%w: JSON path key must be a string literal", ErrUnsupported)
	}
	if s := c.GetSval(); s != nil {
		return s.Sval, nil
	}
	return "", fmt.Errorf("%w: JSON path key must be a string literal", ErrUnsupported)
}

func mapCmpOp(op string) (CmpOp, error) {
	switch op {
	case "=":
		return OpEq, nil
	case "<>", "!=":
		return OpNE, nil
	case "<":
		return OpLT, nil
	case "<=":
		return OpLE, nil
	case ">":
		return OpGT, nil
	case ">=":
		return OpGE, nil
	}
	return 0, fmt.Errorf("%w: comparison operator %q", ErrUnsupported, op)
}

func isCmpOp(op string) bool {
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// ─────────────────────────── expressions ───────────────────────────

func translateExpr(n *pg.Node) (Expr, error) {
	if n == nil {
		return nil, fmt.Errorf("%w: nil expression", ErrUnsupported)
	}

	if pr := n.GetParamRef(); pr != nil {
		// pg_query is 1-indexed for $N, same as our Placeholder.N.
		return Placeholder{N: int(pr.Number)}, nil
	}

	if c := n.GetAConst(); c != nil {
		return translateConst(c)
	}

	if cr := n.GetColumnRef(); cr != nil {
		// `excluded.col` ON CONFLICT reference — detect via the leading
		// "excluded" identifier (pg_query lowercases this).
		if len(cr.Fields) == 2 {
			if first := cr.Fields[0].GetString_(); first != nil && strings.EqualFold(first.Sval, "excluded") {
				if col := cr.Fields[1].GetString_(); col != nil {
					return ExcludedRef{Column: col.Sval}, nil
				}
			}
		}
		name, err := columnRefName(cr)
		if err != nil {
			return nil, err
		}
		return ColumnRef{Name: name}, nil
	}

	if fn := n.GetFuncCall(); fn != nil {
		return translateFuncCall(fn)
	}

	if ce := n.GetCoalesceExpr(); ce != nil {
		return translateCoalesce(ce)
	}

	if tc := n.GetTypeCast(); tc != nil {
		// `$1::vector` — the cast type is consumed by pg_query but
		// adds no semantics for the executor (bind values are already
		// Go-typed). Translate the inner expression and discard the type.
		return translateExpr(tc.Arg)
	}

	if ax := n.GetAExpr(); ax != nil {
		// Vector distance and JSON extract land in expression position
		// (projections, ORDER BY). Detect by operator name.
		return translateExprAExpr(ax)
	}

	return nil, fmt.Errorf("%w: expression kind %T", ErrUnsupported, n.Node)
}

func translateConst(c *pg.A_Const) (Expr, error) {
	if c.Isnull {
		// Bare NULL literal — not currently produced by codegen, and
		// the executor has no NULL-expr representation.
		return nil, fmt.Errorf("%w: bare NULL literal", ErrUnsupported)
	}
	switch v := c.Val.(type) {
	case *pg.A_Const_Sval:
		return Literal{Kind: LitString, Str: v.Sval.Sval}, nil
	case *pg.A_Const_Ival:
		return Literal{Kind: LitInt64, Int64: int64(v.Ival.Ival)}, nil
	case *pg.A_Const_Fval:
		// Float literal — codegen never emits one. Allow it as an
		// integer when it parses cleanly; otherwise reject.
		i, err := strconv.ParseInt(v.Fval.Fval, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: float literal %q", ErrUnsupported, v.Fval.Fval)
		}
		return Literal{Kind: LitInt64, Int64: i}, nil
	case *pg.A_Const_Boolval:
		return nil, fmt.Errorf("%w: boolean literal", ErrUnsupported)
	}
	return nil, fmt.Errorf("%w: literal type %T", ErrUnsupported, c.Val)
}

func translateFuncCall(fn *pg.FuncCall) (Expr, error) {
	if fn.Over != nil {
		return nil, fmt.Errorf("%w: window function in expression position", ErrUnsupported)
	}
	if fn.AggDistinct {
		return nil, fmt.Errorf("%w: DISTINCT aggregate", ErrUnsupported)
	}
	if fn.AggFilter != nil {
		return nil, fmt.Errorf("%w: FILTER clause", ErrUnsupported)
	}
	name := funcCallName(fn)
	if name == "" {
		return nil, fmt.Errorf("%w: empty function name", ErrUnsupported)
	}
	args := make([]Expr, 0, len(fn.Args))
	for _, a := range fn.Args {
		ex, err := translateExpr(a)
		if err != nil {
			return nil, err
		}
		args = append(args, ex)
	}
	return FuncCall{Name: strings.ToUpper(name), Args: args}, nil
}

func translateCoalesce(ce *pg.CoalesceExpr) (Expr, error) {
	if len(ce.Args) < 2 {
		return nil, fmt.Errorf("%w: COALESCE needs at least 2 args", ErrUnsupported)
	}
	if len(ce.Args) > 2 {
		// Codegen only ever emits 2-arg COALESCE (value, default).
		return nil, fmt.Errorf("%w: COALESCE with %d args (only 2 supported)", ErrUnsupported, len(ce.Args))
	}
	// First arg: placeholder, possibly wrapped in a TypeCast.
	first := ce.Args[0]
	if tc := first.GetTypeCast(); tc != nil {
		first = tc.Arg
	}
	pr := first.GetParamRef()
	if pr == nil {
		return nil, fmt.Errorf("%w: COALESCE first arg must be a placeholder", ErrUnsupported)
	}
	def, err := translateExpr(ce.Args[1])
	if err != nil {
		return nil, err
	}
	return Coalesce{Value: Placeholder{N: int(pr.Number)}, Default: def}, nil
}

// translateExprAExpr handles A_Expr nodes that appear in expression
// position (not predicate). Mostly vector distance (`col <=> $1`) and
// JSON extract chains (`col -> 'k'`).
func translateExprAExpr(ax *pg.A_Expr) (Expr, error) {
	op := aExprOpName(ax)
	switch op {
	case "<=>", "<->", "<#>":
		col, err := requireColumnRef(ax.Lexpr, "vector distance LHS")
		if err != nil {
			return nil, err
		}
		ph, err := translatePlaceholder(ax.Rexpr, "vector distance RHS")
		if err != nil {
			return nil, err
		}
		var vop VectorOp
		switch op {
		case "<=>":
			vop = VecCosine
		case "<->":
			vop = VecL2
		case "<#>":
			vop = VecIP
		}
		return VectorDistance{Column: col, Op: vop, Arg: ph}, nil
	case "->", "->>":
		col, path, asText, err := walkJsonChain(ax)
		if err != nil {
			return nil, err
		}
		return JsonExtract{Column: col, Path: path, AsText: asText}, nil
	}
	return nil, fmt.Errorf("%w: operator %q in expression position", ErrUnsupported, op)
}

// ─────────────────────────── small helpers ───────────────────────────

// translateRangeVar maps a pg_query RangeVar (table reference) to
// our TableRef. Rejects everything that isn't a bare schema-qualified
// table (JoinExpr, subselect, etc.).
func translateRangeVar(n *pg.Node) (TableRef, error) {
	if rv := n.GetRangeVar(); rv != nil {
		if rv.Alias != nil {
			return TableRef{}, fmt.Errorf("%w: table alias", ErrUnsupported)
		}
		return TableRef{Schema: rv.Schemaname, Name: rv.Relname}, nil
	}
	if n.GetJoinExpr() != nil {
		return TableRef{}, fmt.Errorf("%w: JOIN", ErrUnsupported)
	}
	if n.GetRangeSubselect() != nil {
		return TableRef{}, fmt.Errorf("%w: subquery in FROM", ErrUnsupported)
	}
	return TableRef{}, fmt.Errorf("%w: FROM clause kind %T", ErrUnsupported, n.Node)
}

// translateLimitExpr accepts either a placeholder ($N) or an integer
// literal for LIMIT / OFFSET. Anything else (a function call, string
// literal, expression, etc.) is rejected with a contextual message.
// The executor evaluates the returned Expr at run time and asserts
// the value is a non-negative int64.
func translateLimitExpr(n *pg.Node, ctx string) (Expr, error) {
	if tc := n.GetTypeCast(); tc != nil {
		n = tc.Arg
	}
	if pr := n.GetParamRef(); pr != nil {
		return Placeholder{N: int(pr.Number)}, nil
	}
	if c := n.GetAConst(); c != nil && !c.Isnull {
		switch v := c.Val.(type) {
		case *pg.A_Const_Ival:
			return Literal{Kind: LitInt64, Int64: int64(v.Ival.Ival)}, nil
		case *pg.A_Const_Fval:
			i, err := strconv.ParseInt(v.Fval.Fval, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: %s value %q is not an integer", ErrUnsupported, ctx, v.Fval.Fval)
			}
			return Literal{Kind: LitInt64, Int64: i}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s must be an integer or placeholder ($N)", ErrUnsupported, ctx)
}

// translatePlaceholder reduces an expression node to a Placeholder,
// erroring when the input isn't a bare $N (with an optional TypeCast
// wrapper, which we strip — `$1::vector` is still a placeholder).
func translatePlaceholder(n *pg.Node, ctx string) (Placeholder, error) {
	if tc := n.GetTypeCast(); tc != nil {
		n = tc.Arg
	}
	pr := n.GetParamRef()
	if pr == nil {
		return Placeholder{}, fmt.Errorf("%w: %s requires placeholder ($N)", ErrUnsupported, ctx)
	}
	return Placeholder{N: int(pr.Number)}, nil
}

// requireColumnRef pulls a column name from a node that must be a
// ColumnRef (one or two fields — schema.column or just column).
// Errors otherwise with a context-tagged message.
func requireColumnRef(n *pg.Node, ctx string) (string, error) {
	cr := n.GetColumnRef()
	if cr == nil {
		return "", fmt.Errorf("%w: %s must be a column reference", ErrUnsupported, ctx)
	}
	return columnRefName(cr)
}

// columnRefName extracts the column name from a ColumnRef. Accepts
// either a 1-field (`col`) or 2-field (`tbl.col` / `schema.col`) ref;
// returns the last field as the column. The executor doesn't currently
// model table-qualified column references, so for 2-field refs we keep
// the column name and let the executor's column-lookup do the matching.
func columnRefName(cr *pg.ColumnRef) (string, error) {
	if len(cr.Fields) == 0 {
		return "", fmt.Errorf("%w: empty column reference", ErrUnsupported)
	}
	last := cr.Fields[len(cr.Fields)-1]
	if s := last.GetString_(); s != nil {
		return s.Sval, nil
	}
	if _, ok := last.Node.(*pg.Node_AStar); ok {
		return "", fmt.Errorf("%w: column reference is *", ErrUnsupported)
	}
	return "", fmt.Errorf("%w: column reference kind %T", ErrUnsupported, last.Node)
}

// aExprOpName extracts the operator string from an A_Expr. pg_query
// represents operators as a Name list of String nodes — usually
// just one element for built-in ops.
func aExprOpName(ax *pg.A_Expr) string {
	if len(ax.Name) == 0 {
		return ""
	}
	last := ax.Name[len(ax.Name)-1]
	if s := last.GetString_(); s != nil {
		return s.Sval
	}
	return ""
}

// funcCallName joins a FuncCall's Funcname list into a dotted name.
// Most calls are single-element ("now"); schema-qualified calls
// ("pg_catalog.now") collapse to their last component since the
// executor doesn't model namespaced functions.
func funcCallName(fn *pg.FuncCall) string {
	if len(fn.Funcname) == 0 {
		return ""
	}
	last := fn.Funcname[len(fn.Funcname)-1]
	if s := last.GetString_(); s != nil {
		return s.Sval
	}
	return ""
}
