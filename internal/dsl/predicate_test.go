package dsl

import (
	"encoding/json"
	"testing"
)

// wherePred parses a single entity carrying one `index partial ... where <pred>`
// and returns the lowered IR predicate tree.
func wherePred(t *testing.T, pred string) *PredExpr {
	t.Helper()
	ir := mustLower(t, `entity A in x {
  id     bigint primary
  status text
  tier   int
  price  double
  is_default boolean
  deleted_at timestamptz
  index partial by id where `+pred+`
}`)
	return ir.Entities[0].Indexes[0].Where
}

func TestParsePredicate_Grammar(t *testing.T) {
	// precedence: `or` binds looser than `and` → (or (and status=.., tier>3) is_default)
	p := wherePred(t, `status = "a" and tier > 3 or is_default`)
	if p.Kind != PredKindBool || p.Op != "or" || len(p.Operands) != 2 {
		t.Fatalf("top should be 2-ary OR, got %+v", p)
	}
	if p.Operands[0].Kind != PredKindBool || p.Operands[0].Op != "and" {
		t.Errorf("left of OR should be AND, got %+v", p.Operands[0])
	}
	if p.Operands[1].Kind != PredKindTruthy {
		t.Errorf("right of OR should be truthy, got %+v", p.Operands[1])
	}

	// flattening: a and b and c → one 3-ary AND
	if a := wherePred(t, `is_default and tier > 1 and status = "x"`); a.Kind != PredKindBool || len(a.Operands) != 3 {
		t.Errorf("a and b and c should flatten to 3-ary AND, got %+v", a)
	}

	// parens override precedence: (a or b) and c
	if a := wherePred(t, `(is_default or tier > 1) and status = "x"`); a.Kind != PredKindBool || a.Op != "and" || a.Operands[0].Op != "or" {
		t.Errorf("paren grouping wrong, got %+v", a)
	}

	// not, in / not in, float, bare bool
	if n := wherePred(t, `not is_default`); n.Kind != PredKindNot || n.Inner.Kind != PredKindTruthy {
		t.Errorf("not bare-bool wrong: %+v", n)
	}
	if in := wherePred(t, `tier in (1, 2, 3)`); in.Kind != PredKindIn || in.Negated || len(in.List) != 3 {
		t.Errorf("in-list wrong: %+v", in)
	}
	if in := wherePred(t, `tier not in (1, 2)`); in.Kind != PredKindIn || !in.Negated {
		t.Errorf("not-in wrong: %+v", in)
	}
	if c := wherePred(t, `price > 3.14`); c.Kind != PredKindCompare || c.Right.Literal.Kind != DefaultIRFloat || c.Right.Literal.Float != 3.14 {
		t.Errorf("float literal wrong: %+v", c.Right)
	}
	if b := wherePred(t, `is_default`); b.Kind != PredKindTruthy {
		t.Errorf("bare bool wrong: %+v", b)
	}
}

func TestParsePredicate_FuncCast(t *testing.T) {
	// function call as an operand
	f := wherePred(t, `lower(status) = "x"`)
	if f.Kind != PredKindCompare || f.Left.Kind != OperandFunc || f.Left.FuncName != "lower" {
		t.Fatalf("function operand wrong: %+v", f.Left)
	}
	if len(f.Left.Args) != 1 || f.Left.Args[0].Kind != OperandColumn || f.Left.Args[0].Name != "status" {
		t.Errorf("function arg wrong: %+v", f.Left.Args)
	}

	// multi-arg function
	c := wherePred(t, `coalesce(status, "n") = "x"`)
	if c.Left.Kind != OperandFunc || len(c.Left.Args) != 2 {
		t.Errorf("coalesce args wrong: %+v", c.Left)
	}

	// postfix cast, including a parameterized type
	ct := wherePred(t, `tier::bigint > 3`)
	if ct.Kind != PredKindCompare || ct.Left.Kind != OperandCast || ct.Left.CastType != "bigint" {
		t.Fatalf("cast operand wrong: %+v", ct.Left)
	}
	if ct.Left.Inner == nil || ct.Left.Inner.Kind != OperandColumn || ct.Left.Inner.Name != "tier" {
		t.Errorf("cast inner wrong: %+v", ct.Left.Inner)
	}
	if pc := wherePred(t, `status::varchar(20) = "x"`); pc.Left.CastType != "varchar(20)" {
		t.Errorf("parameterized cast type wrong: %q", pc.Left.CastType)
	}

	// every referenced column is collected (function args + cast inner)
	if cols := wherePred(t, `lower(status) = "x" and tier::bigint > 1`).Columns(); len(cols) != 2 {
		t.Errorf("expected 2 referenced columns, got %v", cols)
	}
}

func TestParsePredicate_Case(t *testing.T) {
	w := wherePred(t, `case when is_default then tier else 0 end > 0`)
	if w.Kind != PredKindCompare || w.Op != ">" || w.Left.Kind != OperandCase {
		t.Fatalf("case-in-comparison wrong: %+v", w)
	}
	c := w.Left
	if len(c.Whens) != 1 || c.Whens[0].Cond.Kind != PredKindTruthy {
		t.Errorf("case arm/cond wrong: %+v", c.Whens)
	}
	if c.Whens[0].Then.Kind != OperandColumn || c.Whens[0].Then.Name != "tier" {
		t.Errorf("case THEN wrong: %+v", c.Whens[0].Then)
	}
	if c.Else == nil || c.Else.Literal == nil || c.Else.Literal.Int != 0 {
		t.Errorf("case ELSE wrong: %+v", c.Else)
	}
	// CASE condition columns are collected for validation.
	if cols := wherePred(t, `case when is_default then tier else 0 end > 0`).Columns(); len(cols) != 2 {
		t.Errorf("expected is_default+tier collected, got %v", cols)
	}
	// `case` is still usable as a column name when not followed by `when`.
	if p := wherePred(t, `tier > 0`); p.Kind != PredKindCompare {
		t.Errorf("sanity: plain comparison broke: %+v", p)
	}
}

// legacyRef mirrors the exact JSON shape of the pre-tree PartialPred struct.
// Marshaling a legacy PredExpr must equal marshaling the equivalent legacyRef,
// byte-for-byte — that is the ir_checkpoint compatibility contract (independent
// of encoding/json's HTML escaping, which both sides share).
type legacyRef struct {
	Field   string   `json:"field"`
	IsNull  bool     `json:"is_null,omitempty"`
	Op      string   `json:"op,omitempty"`
	Literal *Default `json:"literal,omitempty"`
}

func TestPredExprJSON_LegacyByteCompat(t *testing.T) {
	cases := []struct {
		pred string
		ref  legacyRef
	}{
		{`deleted_at is null`, legacyRef{Field: "deleted_at", IsNull: true}},
		{`deleted_at is not null`, legacyRef{Field: "deleted_at"}},
		{`status = "active"`, legacyRef{Field: "status", Op: "=", Literal: &Default{Kind: DefaultIRString, Str: "active"}}},
		{`tier >= 3`, legacyRef{Field: "tier", Op: ">=", Literal: &Default{Kind: DefaultIRInt, Int: 3}}},
	}
	for _, tc := range cases {
		got, err := json.Marshal(wherePred(t, tc.pred))
		if err != nil {
			t.Fatalf("marshal %q: %v", tc.pred, err)
		}
		want, _ := json.Marshal(tc.ref)
		if string(got) != string(want) {
			t.Errorf("legacy %q marshaled to %s, want %s", tc.pred, got, want)
		}
		// round-trip: old flat JSON unmarshals and re-marshals identically.
		var back PredExpr
		if err := json.Unmarshal(want, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", want, err)
		}
		if re, _ := json.Marshal(&back); string(re) != string(want) {
			t.Errorf("round-trip %s -> %s", want, re)
		}
	}

	// compound predicates take the tagged form (contains "kind").
	got, _ := json.Marshal(wherePred(t, `is_default and deleted_at is null`))
	var probe map[string]any
	_ = json.Unmarshal(got, &probe)
	if probe["kind"] != "bool" {
		t.Errorf("compound predicate should marshal tagged, got %s", got)
	}
	// and unmarshals back to an identical tree.
	var back PredExpr
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("compound unmarshal: %v", err)
	}
	if re, _ := json.Marshal(&back); string(re) != string(got) {
		t.Errorf("compound round-trip mismatch: %s vs %s", got, re)
	}
}
