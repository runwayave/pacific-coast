package introspect

import (
	"sort"
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// normalizedEqual reports whether two ALREADY-Postgres-normalized predicate
// deparses (one from the live index, one from normalizePredicate) denote the
// same predicate. Postgres has already resolved casts, operators, IN→ANY, and
// function/type coercion identically on both sides, so the ONLY way two
// equivalent predicates can still differ textually is the source order of a
// commutative operator (`a OR b` vs `b OR a`, `ARRAY[1,2]` vs `ARRAY[2,1]`).
//
// So the comparison is: fast path string equality, else compare canonical forms
// that differ from the deparse in exactly one respect — operands of `AND`/`OR`
// and the element list of an `= ANY(ARRAY[...])`/`IN` are sorted. Sorting the
// operands of a commutative operator is the only normalization applied, and it
// is trivially equivalence-preserving, so this can never accept two predicates
// Postgres considers different. An unrecognised node makes the canonical form
// unavailable, so a textual difference there stays drift (the safe direction).
func normalizedEqual(a, b string) bool {
	if a == b {
		return true
	}
	na, nb := parseWhereExpr(a), parseWhereExpr(b)
	if na == nil || nb == nil {
		return false
	}
	ca, oka := canonForm(na)
	cb, okb := canonForm(nb)
	return oka && okb && ca == cb
}

// parseWhereExpr parses a bare predicate by wrapping it in a trivial SELECT and
// returning the WHERE node, or nil if it doesn't parse.
func parseWhereExpr(text string) *pg.Node {
	tree, err := pg.Parse("SELECT 1 WHERE " + text)
	if err != nil || len(tree.Stmts) != 1 {
		return nil
	}
	sel := tree.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil {
		return nil
	}
	return sel.WhereClause
}

// canonForm faithfully serialises a Postgres-normalized predicate node, sorting
// only the operands of commutative operators. ok=false on an unrecognised node.
func canonForm(n *pg.Node) (string, bool) {
	if n == nil {
		return "", false
	}

	if be := n.GetBoolExpr(); be != nil {
		switch be.Boolop {
		case pg.BoolExprType_AND_EXPR, pg.BoolExprType_OR_EXPR:
			op := "and"
			if be.Boolop == pg.BoolExprType_OR_EXPR {
				op = "or"
			}
			parts, ok := canonList(be.Args)
			if !ok {
				return "", false
			}
			sort.Strings(parts) // commutative
			return "(" + op + " " + strings.Join(parts, " ") + ")", true
		case pg.BoolExprType_NOT_EXPR:
			if len(be.Args) != 1 {
				return "", false
			}
			c, ok := canonForm(be.Args[0])
			if !ok {
				return "", false
			}
			return "(not " + c + ")", true
		}
		return "", false
	}

	if ax := n.GetAExpr(); ax != nil {
		op := aExprOp(ax)
		switch ax.Kind {
		case pg.A_Expr_Kind_AEXPR_OP:
			l, lok := canonForm(ax.Lexpr)
			r, rok := canonForm(ax.Rexpr)
			if !lok || !rok {
				return "", false
			}
			return "(op " + op + " " + l + " " + r + ")", true
		case pg.A_Expr_Kind_AEXPR_OP_ANY:
			arr := ax.Rexpr.GetAArrayExpr()
			if arr == nil {
				return "", false
			}
			return canonAnyIn("any:"+op, ax.Lexpr, arr.GetElements())
		case pg.A_Expr_Kind_AEXPR_IN:
			return canonAnyIn("in:"+op, ax.Lexpr, ax.Rexpr.GetList().GetItems())
		}
		return "", false
	}

	if nt := n.GetNullTest(); nt != nil {
		c, ok := canonForm(nt.Arg)
		if !ok {
			return "", false
		}
		return "(null " + strconv.FormatBool(nt.Nulltesttype == pg.NullTestType_IS_NULL) + " " + c + ")", true
	}

	if bt := n.GetBooleanTest(); bt != nil {
		c, ok := canonForm(bt.Arg)
		if !ok {
			return "", false
		}
		return "(booltest " + strconv.Itoa(int(bt.Booltesttype)) + " " + c + ")", true
	}

	if tc := n.GetTypeCast(); tc != nil {
		c, ok := canonForm(tc.Arg)
		if !ok {
			return "", false
		}
		return "(cast " + c + " " + typeName(tc.TypeName) + ")", true
	}

	if fc := n.GetFuncCall(); fc != nil {
		// Function args are positional. The name is whatever Postgres deparsed
		// (lower-cased, possibly schema-qualified) — identical on both sides.
		args, ok := canonList(fc.Args)
		if !ok {
			return "", false
		}
		return "(func " + funcName(fc.Funcname) + " " + strings.Join(args, " ") + ")", true
	}

	if ce := n.GetCoalesceExpr(); ce != nil {
		args, ok := canonList(ce.Args)
		if !ok {
			return "", false
		}
		return "(coalesce " + strings.Join(args, " ") + ")", true
	}

	if ca := n.GetCaseExpr(); ca != nil {
		// CASE arms are order-significant — keep positional.
		arms := make([]string, 0, len(ca.Args))
		for _, a := range ca.Args {
			cw := a.GetCaseWhen()
			if cw == nil {
				return "", false
			}
			cond, ok1 := canonForm(cw.Expr)
			res, ok2 := canonForm(cw.Result)
			if !ok1 || !ok2 {
				return "", false
			}
			arms = append(arms, "(when "+cond+" "+res+")")
		}
		arg := ""
		if ca.Arg != nil {
			c, ok := canonForm(ca.Arg)
			if !ok {
				return "", false
			}
			arg = c
		}
		els := ""
		if ca.Defresult != nil {
			c, ok := canonForm(ca.Defresult)
			if !ok {
				return "", false
			}
			els = c
		}
		return "(case " + arg + " " + strings.Join(arms, " ") + " else " + els + ")", true
	}

	if cr := n.GetColumnRef(); cr != nil {
		if name, ok := columnRefName(n); ok {
			return "col:" + name, true
		}
		return "", false
	}

	if c := n.GetAConst(); c != nil {
		return constForm(c)
	}

	if arr := n.GetAArrayExpr(); arr != nil {
		// A bare array (not an IN/ANY) is order-significant — keep positional.
		parts, ok := canonList(arr.Elements)
		if !ok {
			return "", false
		}
		return "(array " + strings.Join(parts, " ") + ")", true
	}

	return "", false
}

// canonAnyIn serialises an IN / `= ANY(ARRAY[...])` with its element list sorted
// (set membership is order-independent).
func canonAnyIn(tag string, arg *pg.Node, elems []*pg.Node) (string, bool) {
	a, ok := canonForm(arg)
	if !ok {
		return "", false
	}
	parts, ok := canonList(elems)
	if !ok {
		return "", false
	}
	sort.Strings(parts)
	return "(" + tag + " " + a + " [" + strings.Join(parts, " ") + "])", true
}

func canonList(nodes []*pg.Node) ([]string, bool) {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		c, ok := canonForm(n)
		if !ok {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}

func constForm(c *pg.A_Const) (string, bool) {
	if c.Isnull {
		return "const:null", true
	}
	switch v := c.Val.(type) {
	case *pg.A_Const_Sval:
		return "const:s:" + v.Sval.Sval, true
	case *pg.A_Const_Ival:
		return "const:i:" + strconv.FormatInt(int64(v.Ival.Ival), 10), true
	case *pg.A_Const_Fval:
		return "const:f:" + v.Fval.Fval, true
	case *pg.A_Const_Boolval:
		return "const:b:" + strconv.FormatBool(v.Boolval.Boolval), true
	}
	return "", false
}

func typeName(tn *pg.TypeName) string {
	if tn == nil {
		return ""
	}
	return joinNames(tn.Names)
}

func funcName(names []*pg.Node) string { return joinNames(names) }

func joinNames(names []*pg.Node) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if s := n.GetString_(); s != nil {
			parts = append(parts, s.Sval)
		}
	}
	return strings.Join(parts, ".")
}

func columnRefName(n *pg.Node) (string, bool) {
	cr := n.GetColumnRef()
	if cr == nil || len(cr.Fields) == 0 {
		return "", false
	}
	last := cr.Fields[len(cr.Fields)-1].GetString_()
	if last == nil {
		return "", false
	}
	return last.Sval, true
}

func aExprOp(ax *pg.A_Expr) string {
	if len(ax.Name) == 0 {
		return ""
	}
	s := ax.Name[len(ax.Name)-1].GetString_()
	if s == nil {
		return ""
	}
	return s.Sval
}
