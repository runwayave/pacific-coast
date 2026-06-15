// Package predsql renders a resolved partial-index predicate (dsl.PredExpr) to
// SQL and to a stable diff-identity key. It is a leaf shared by two callers that
// must not depend on each other: codegen (emits CREATE INDEX ... WHERE) and
// introspect (renders the declared predicate so the drift matcher can compare it
// against the live one through pg_query). Keeping a single renderer here is what
// guarantees the emitted predicate and the matched predicate never diverge.
package predsql

import (
	"sort"
	"strconv"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Render returns the SQL boolean expression for a predicate tree, with no
// leading WHERE. Parentheses are emitted around nested boolean nodes so the
// string re-parses to the same tree regardless of the source's grouping.
func Render(p *dsl.PredExpr) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case dsl.PredKindBool:
		sep := " AND "
		if p.Op == "or" {
			sep = " OR "
		}
		parts := make([]string, len(p.Operands))
		for i, o := range p.Operands {
			parts[i] = renderChild(o)
		}
		return strings.Join(parts, sep)
	case dsl.PredKindNot:
		return "NOT (" + Render(p.Inner) + ")"
	case dsl.PredKindCompare:
		return renderOperand(p.Left) + " " + sqlOp(p.Op) + " " + renderOperand(p.Right)
	case dsl.PredKindNull:
		if p.Negated {
			return renderOperand(p.Arg) + " IS NOT NULL"
		}
		return renderOperand(p.Arg) + " IS NULL"
	case dsl.PredKindIn:
		kw := " IN ("
		if p.Negated {
			kw = " NOT IN ("
		}
		elems := make([]string, len(p.List))
		for i, o := range p.List {
			elems[i] = renderOperand(o)
		}
		return renderOperand(p.Arg) + kw + strings.Join(elems, ", ") + ")"
	case dsl.PredKindTruthy:
		return renderOperand(p.Arg)
	}
	return ""
}

// renderChild parenthesises a nested boolean operand so precedence survives.
func renderChild(p *dsl.PredExpr) string {
	if p != nil && p.Kind == dsl.PredKindBool {
		return "(" + Render(p) + ")"
	}
	return Render(p)
}

func renderOperand(o *dsl.PredOperand) string {
	if o == nil {
		return ""
	}
	switch o.Kind {
	case dsl.OperandColumn:
		return schema.QuoteIdent(o.Name)
	case dsl.OperandLiteral:
		if o.Literal != nil {
			return schema.DefaultExpr(*o.Literal)
		}
	case dsl.OperandFunc:
		args := make([]string, len(o.Args))
		for i, a := range o.Args {
			args[i] = renderOperand(a)
		}
		return o.FuncName + "(" + strings.Join(args, ", ") + ")"
	case dsl.OperandCast:
		return renderOperand(o.Inner) + "::" + o.CastType
	case dsl.OperandCase:
		var b strings.Builder
		b.WriteString("CASE")
		for _, w := range o.Whens {
			b.WriteString(" WHEN " + Render(w.Cond) + " THEN " + renderOperand(w.Then))
		}
		if o.Else != nil {
			b.WriteString(" ELSE " + renderOperand(o.Else))
		}
		b.WriteString(" END")
		return b.String()
	}
	return ""
}

// sqlOp emits the Postgres-canonical operator (`!=` deparses as `<>`).
func sqlOp(op string) string {
	if op == "!=" {
		return "<>"
	}
	return op
}

// CanonicalKey returns the predicate portion of an index's diff-identity key,
// prefixed with `|`. For the two pre-tree shapes it reproduces the exact bytes
// the diff engine produced before the predicate became a tree, so an
// already-applied index never re-diffs as drop+recreate. Compound predicates get
// a deterministic structural encoding that cannot collide with a legacy string
// (legacy keys continue with a column name; structural keys continue with `(`).
func CanonicalKey(p *dsl.PredExpr) string {
	if p == nil {
		return ""
	}
	if field, op, isNull, lit, ok := p.LegacyForm(); ok {
		if op == "" {
			if isNull {
				return "|" + field + " is null"
			}
			return "|" + field + " is not null"
		}
		return "|" + field + " " + op + legacyLiteralTag(lit)
	}
	return "|" + structuralKey(p)
}

// legacyLiteralTag reproduces the diff engine's pre-tree literal encoding
// (" s:"/" i:"/" b:") exactly. Only string/int/bool ever reach here (LegacyForm
// excludes float literals).
func legacyLiteralTag(d *dsl.Default) string {
	if d == nil {
		return ""
	}
	switch d.Kind {
	case dsl.DefaultIRString:
		return " s:" + d.Str
	case dsl.DefaultIRInt:
		return " i:" + strconv.FormatInt(d.Int, 10)
	case dsl.DefaultIRBool:
		return " b:" + strconv.FormatBool(d.Bool)
	}
	return ""
}

// structuralKey is the deterministic, commutativity-stable encoding for compound
// predicates. Boolean and IN operands are sorted so `a and b` and `b and a`
// collapse to one key (matching the drift matcher's multiset semantics).
func structuralKey(p *dsl.PredExpr) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case dsl.PredKindBool:
		parts := make([]string, len(p.Operands))
		for i, o := range p.Operands {
			parts[i] = structuralKey(o)
		}
		sort.Strings(parts)
		return "(" + p.Op + " " + strings.Join(parts, " ") + ")"
	case dsl.PredKindNot:
		return "(not " + structuralKey(p.Inner) + ")"
	case dsl.PredKindCompare:
		return "(" + operandKey(p.Left) + " " + p.Op + " " + operandKey(p.Right) + ")"
	case dsl.PredKindNull:
		if p.Negated {
			return "(" + operandKey(p.Arg) + " isnotnull)"
		}
		return "(" + operandKey(p.Arg) + " isnull)"
	case dsl.PredKindIn:
		elems := make([]string, len(p.List))
		for i, o := range p.List {
			elems[i] = operandKey(o)
		}
		sort.Strings(elems)
		kw := " in "
		if p.Negated {
			kw = " notin "
		}
		return "(" + operandKey(p.Arg) + kw + "[" + strings.Join(elems, " ") + "])"
	case dsl.PredKindTruthy:
		return "(truthy " + operandKey(p.Arg) + ")"
	}
	return ""
}

func operandKey(o *dsl.PredOperand) string {
	if o == nil {
		return ""
	}
	switch o.Kind {
	case dsl.OperandColumn:
		return "c:" + o.Name
	case dsl.OperandLiteral:
		return "l:" + literalKey(o.Literal)
	case dsl.OperandFunc:
		parts := make([]string, len(o.Args))
		for i, a := range o.Args {
			parts[i] = operandKey(a)
		}
		return "fn:" + o.FuncName + "(" + strings.Join(parts, ",") + ")"
	case dsl.OperandCast:
		return "cast:" + operandKey(o.Inner) + "::" + o.CastType
	case dsl.OperandCase:
		var b strings.Builder
		b.WriteString("case:[")
		for _, w := range o.Whens {
			b.WriteString("(when " + structuralKey(w.Cond) + " " + operandKey(w.Then) + ")")
		}
		b.WriteString("]else:")
		if o.Else != nil {
			b.WriteString(operandKey(o.Else))
		}
		return b.String()
	}
	return ""
}

func literalKey(d *dsl.Default) string {
	if d == nil {
		return ""
	}
	switch d.Kind {
	case dsl.DefaultIRString:
		return "s:" + d.Str
	case dsl.DefaultIRInt:
		return "i:" + strconv.FormatInt(d.Int, 10)
	case dsl.DefaultIRFloat:
		return "f:" + strconv.FormatFloat(d.Float, 'g', -1, 64)
	case dsl.DefaultIRBool:
		return "b:" + strconv.FormatBool(d.Bool)
	}
	return ""
}
