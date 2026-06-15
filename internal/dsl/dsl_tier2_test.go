package dsl

import (
	"strings"
	"testing"
)

// Coverage for the Tier 2 DSL additions:
//   - default raw "..."
//   - identity / check on fields
//   - composite UNIQUE and composite PRIMARY KEY at member position
//   - soft_delete by ...
//   - richer partial-index predicates (comparison ops)
//   - expression indexes (expr "lower(email)")
//
// Pure parse + lower assertions — the SQL emitter shape is verified
// separately in internal/codegen/sql_test.go.

func TestTier2_DefaultRaw(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id   bigint primary
  meta jsonb default raw "'{}'::jsonb"
}`)
	meta := ir.Entities[0].FindField("meta")
	if meta == nil || meta.Default == nil {
		t.Fatal("meta default missing")
	}
	if meta.Default.Kind != DefaultIRRaw {
		t.Errorf("default kind: got %v want raw", meta.Default.Kind)
	}
	if !strings.Contains(meta.Default.Str, "jsonb") {
		t.Errorf("raw expr lost: %q", meta.Default.Str)
	}
}

func TestTier2_IdentityAndCheck(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id    bigint primary identity
  qty   int not null check "qty > 0"
}`)
	id := ir.Entities[0].FindField("id")
	if id == nil || !id.Identity {
		t.Errorf("identity flag missing on id")
	}
	qty := ir.Entities[0].FindField("qty")
	if qty == nil || qty.Check == "" {
		t.Errorf("check expr missing on qty")
	}
}

func TestTier2_CompositeUnique(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id    bigint primary
  vendor bigint
  sku   text
  unique by vendor, sku
}`)
	e := ir.Entities[0]
	if len(e.Uniques) != 1 || len(e.Uniques[0].Fields) != 2 {
		t.Fatalf("composite unique not captured: %+v", e.Uniques)
	}
	if e.Uniques[0].Fields[0] != "vendor" || e.Uniques[0].Fields[1] != "sku" {
		t.Errorf("unique fields wrong: %v", e.Uniques[0].Fields)
	}
}

func TestTier2_CompositePK(t *testing.T) {
	ir := mustLower(t, `entity AB in x {
  a bigint
  b bigint
  primary by a, b
}`)
	if len(ir.Entities[0].CompositePK) != 2 {
		t.Errorf("composite pk not captured: %v", ir.Entities[0].CompositePK)
	}
}

func TestTier2_SoftDelete(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id         bigint primary
  deleted_at timestamptz
  soft_delete by deleted_at
}`)
	if ir.Entities[0].SoftDeleteField != "deleted_at" {
		t.Errorf("soft_delete field not set: %q", ir.Entities[0].SoftDeleteField)
	}
}

func TestTier2_SoftDelete_RejectsNotNull(t *testing.T) {
	// Active rows must carry NULL — a NOT NULL deleted_at is nonsense.
	err := mustLowerErr(t, `entity A in x {
  id         bigint primary
  deleted_at timestamptz not null
  soft_delete by deleted_at
}`)
	if !strings.Contains(err.Error(), "must be nullable") {
		t.Errorf("expected nullable-soft-delete error, got %v", err)
	}
}

func TestTier2_PartialPredicateComparison(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id     bigint primary
  status text
  index partial by id where status = "active"
}`)
	idx := ir.Entities[0].Indexes[0]
	w := idx.Where
	if w == nil || w.Kind != PredKindCompare || w.Op != "=" {
		t.Fatalf("partial pred op missing: %+v", w)
	}
	if w.Left == nil || w.Left.Kind != OperandColumn || w.Left.Name != "status" {
		t.Errorf("partial pred lhs wrong: %+v", w.Left)
	}
	if w.Right == nil || w.Right.Literal == nil || w.Right.Literal.Str != "active" {
		t.Errorf("partial pred literal wrong: %+v", w.Right)
	}
}

func TestTier2_ExpressionIndex(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id    bigint primary
  email text
  index by expr "lower(email)"
}`)
	idx := ir.Entities[0].Indexes[0]
	if len(idx.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(idx.Fields))
	}
	if !idx.Fields[0].IsExpr || idx.Fields[0].Expr != "lower(email)" {
		t.Errorf("expression index not captured: %+v", idx.Fields[0])
	}
}

func TestTier2_VarcharType(t *testing.T) {
	ir := mustLower(t, `entity A in x {
  id    bigint primary
  sku   varchar(64) unique
}`)
	sku := ir.Entities[0].FindField("sku")
	if sku.Type.Name != "varchar" || sku.Type.Len != 64 {
		t.Errorf("varchar(64) shape wrong: %+v", sku.Type)
	}
}
