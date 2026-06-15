package predsql

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func col(n string) *dsl.PredOperand { return &dsl.PredOperand{Kind: dsl.OperandColumn, Name: n} }
func litS(s string) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: &dsl.Default{Kind: dsl.DefaultIRString, Str: s}}
}
func litI(i int64) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: &dsl.Default{Kind: dsl.DefaultIRInt, Int: i}}
}
func null(n string, neg bool) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindNull, Arg: col(n), Negated: neg}
}
func cmp(op string, l, r *dsl.PredOperand) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindCompare, Op: op, Left: l, Right: r}
}
func and(ops ...*dsl.PredExpr) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindBool, Op: "and", Operands: ops}
}

// TestCanonicalKey_LegacyBytes is the Sev-1 diff-key guard: the two pre-tree
// shapes must produce the exact key bytes the diff engine emitted before
// predicates became trees, or already-applied indexes re-diff as drop+recreate.
func TestCanonicalKey_LegacyBytes(t *testing.T) {
	cases := []struct {
		p    *dsl.PredExpr
		want string
	}{
		{null("deleted_at", false), "|deleted_at is null"},
		{null("deleted_at", true), "|deleted_at is not null"},
		{cmp("=", col("status"), litS("active")), "|status = s:active"},
		{cmp(">=", col("tier"), litI(3)), "|tier >= i:3"},
	}
	for _, tc := range cases {
		if got := CanonicalKey(tc.p); got != tc.want {
			t.Errorf("CanonicalKey = %q, want %q", got, tc.want)
		}
	}
}

// TestCanonicalKey_Commutative ensures reordered AND operands collapse to one
// key (matching the matcher's multiset semantics) so commuting the predicate in
// the .atl is not seen as a change.
func TestCanonicalKey_Commutative(t *testing.T) {
	a := and(null("deleted_at", false), cmp("=", col("status"), litS("active")))
	b := and(cmp("=", col("status"), litS("active")), null("deleted_at", false))
	if CanonicalKey(a) != CanonicalKey(b) {
		t.Errorf("commuted AND keys differ:\n %q\n %q", CanonicalKey(a), CanonicalKey(b))
	}
	// distinct from a single-node legacy key (cannot collide).
	if CanonicalKey(a) == CanonicalKey(null("deleted_at", false)) {
		t.Error("compound key collided with a legacy key")
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		p    *dsl.PredExpr
		want string
	}{
		{null("deleted_at", false), `"deleted_at" IS NULL`},
		{cmp("!=", col("status"), litS("x")), `"status" <> 'x'`},
		{and(null("deleted_at", false), cmp("=", col("status"), litS("a"))),
			`"deleted_at" IS NULL AND "status" = 'a'`},
	}
	for _, tc := range cases {
		if got := Render(tc.p); got != tc.want {
			t.Errorf("Render = %q, want %q", got, tc.want)
		}
	}
}
