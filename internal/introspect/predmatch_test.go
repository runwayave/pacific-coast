package introspect

import "github.com/rachitkumar205/atlantis/internal/dsl"

// Predicate builders shared by the drift tests. The drift matcher itself is now
// the DB normalizer (normalize.go), verified end-to-end against a live Postgres
// in drift_live_test.go; the pure-Go unit tests inject a fake normalizer.

func col(name string) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandColumn, Name: name}
}
func litS(s string) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: &dsl.Default{Kind: dsl.DefaultIRString, Str: s}}
}
func litI(i int64) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: &dsl.Default{Kind: dsl.DefaultIRInt, Int: i}}
}

func nullPred(name string, negated bool) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindNull, Arg: col(name), Negated: negated}
}
func cmpPred(op string, l, r *dsl.PredOperand) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindCompare, Op: op, Left: l, Right: r}
}
func boolPred(op string, ops ...*dsl.PredExpr) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindBool, Op: op, Operands: ops}
}
func truthyPred(name string) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindTruthy, Arg: col(name)}
}
func inPred(name string, negated bool, items ...*dsl.PredOperand) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindIn, Arg: col(name), List: items, Negated: negated}
}
func funcOp(name string, args ...*dsl.PredOperand) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandFunc, FuncName: name, Args: args}
}
func castOp(inner *dsl.PredOperand, typ string) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandCast, Inner: inner, CastType: typ}
}
func caseOp(els *dsl.PredOperand, whens ...dsl.PredCaseWhen) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandCase, Whens: whens, Else: els}
}
func when(cond *dsl.PredExpr, then *dsl.PredOperand) dsl.PredCaseWhen {
	return dsl.PredCaseWhen{Cond: cond, Then: then}
}

// fakeNormalize stands in for the Postgres normalizer in pure-Go drift tests:
// it deparses the simple null shapes the way pg_get_expr would, so the classify
// logic can be exercised without a database. Real deparse fidelity is covered by
// drift_live_test.go.
func fakeNormalize(_ string, p *dsl.PredExpr) (string, bool) {
	if p.Kind == dsl.PredKindNull && p.Arg != nil && p.Arg.Kind == dsl.OperandColumn {
		if p.Negated {
			return "(" + p.Arg.Name + " IS NOT NULL)", true
		}
		return "(" + p.Arg.Name + " IS NULL)", true
	}
	return "", false
}
