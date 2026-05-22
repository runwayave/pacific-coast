package codegen

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func lower(t *testing.T, src string) *dsl.IR {
	t.Helper()
	f, err := dsl.Parse("t.pc", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}

// findChange returns the first change matching kind, or nil if absent.
func findChange(t *testing.T, d *Diff, kind ChangeKind) *Change {
	t.Helper()
	for _, c := range d.Additive {
		if c.Kind == kind {
			return &c
		}
	}
	for _, c := range d.BackfillRequired {
		if c.Kind == kind {
			return &c
		}
	}
	for _, c := range d.Breaking {
		if c.Kind == kind {
			return &c
		}
	}
	return nil
}

func TestDiff_Empty(t *testing.T) {
	if !ComputeDiff(nil, nil).IsEmpty() {
		t.Errorf("nil/nil should be empty")
	}
	ir := lower(t, `entity A in x { id bigint primary }`)
	if !ComputeDiff(ir, ir).IsEmpty() {
		t.Errorf("identical IRs should diff to empty")
	}
}

func TestDiff_EntityAdded(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary }
`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindEntityAdded); c == nil || c.EntityID != "x.B" || c.Class != ClassAdditive {
		t.Errorf("expected additive entity_added for x.B, got %+v", c)
	}
	if d.HighestClass() != ClassAdditive {
		t.Errorf("highest class should be additive, got %v", d.HighestClass())
	}
}

func TestDiff_EntityRemoved_IsBreaking(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary } entity B in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindEntityRemoved); c == nil || c.Class != ClassCrossCallerBreaking {
		t.Errorf("entity removal should be breaking, got %+v", c)
	}
	if d.HighestClass() != ClassCrossCallerBreaking {
		t.Errorf("highest class should be breaking")
	}
}

func TestDiff_FieldAdded_Nullable_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  name text }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldAdded)
	if c == nil || c.Class != ClassAdditive || c.Field != "name" {
		t.Errorf("nullable field add should be additive, got %+v", c)
	}
}

func TestDiff_FieldAdded_NotNull_NoDefault_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  name text not null }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldAdded)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("NOT NULL field add with no default should require backfill, got %+v", c)
	}
}

func TestDiff_FieldAdded_NotNullWithDefault_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  name text not null default "" }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldAdded)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("NOT NULL with DEFAULT should be additive, got %+v", c)
	}
}

func TestDiff_FieldRemoved_IsBreaking(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  name text }`)
	newIR := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldRemoved)
	if c == nil || c.Class != ClassCrossCallerBreaking {
		t.Errorf("field removal should be breaking, got %+v", c)
	}
}

func TestDiff_TypeWidening_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id smallint primary }`)
	newIR := lower(t, `entity A in x { id int primary }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldTypeChanged)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("smallint→int widening should be additive, got %+v", c)
	}
}

func TestDiff_TypeNarrowing_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id int primary }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldTypeChanged)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("bigint→int narrowing should require backfill, got %+v", c)
	}
}

func TestDiff_TypeUnrelated_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldTypeChanged)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("text→int should require backfill, got %+v", c)
	}
}

func TestDiff_VectorDimChange_IsBreaking(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v vector(32) }`)
	newIR := lower(t, `entity A in x { id bigint primary  v vector(64) }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldTypeChanged)
	if c == nil || c.Class != ClassCrossCallerBreaking {
		t.Errorf("vector dim change should be breaking, got %+v", c)
	}
}

func TestDiff_NotNullTightened_NoDefault_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text not null }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldNotNullTightened)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("NOT NULL tightened without default should require backfill, got %+v", c)
	}
}

func TestDiff_NotNullTightened_WithDefault_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text not null default "" }`)
	d := ComputeDiff(oldIR, newIR)
	// NOT NULL tightened is additive when DEFAULT covers existing rows.
	c := findChange(t, d, KindFieldNotNullTightened)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("NOT NULL tightened with default should be additive, got %+v", c)
	}
}

func TestDiff_NotNullLoosened_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text not null }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldNotNullLoosened)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("NOT NULL loosened should be additive, got %+v", c)
	}
}

func TestDiff_UniqueAdded_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  email text }`)
	newIR := lower(t, `entity A in x { id bigint primary  email text unique }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldUniqueAdded)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("UNIQUE added should require backfill (dupes possible), got %+v", c)
	}
}

func TestDiff_UniqueRemoved_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  email text unique }`)
	newIR := lower(t, `entity A in x { id bigint primary  email text }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindFieldUniqueRemoved); c == nil || c.Class != ClassAdditive {
		t.Errorf("UNIQUE removed should be additive, got %+v", c)
	}
}

func TestDiff_DefaultChanged_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v int default 1 }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int default 2 }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindFieldDefaultChanged); c == nil || c.Class != ClassAdditive {
		t.Errorf("DEFAULT change should be additive, got %+v", c)
	}
}

func TestDiff_BackfillAdded_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text backfill "'x'" }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldBackfillAdded)
	if c == nil || c.Class != ClassAdditive || c.Field != "v" {
		t.Errorf("backfill added should be additive on v, got %+v", c)
	}
}

func TestDiff_BackfillRemoved_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text backfill "'x'" }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldBackfillRemoved)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("backfill removed should be additive, got %+v", c)
	}
}

func TestDiff_BackfillChanged_IsAdditive(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text backfill "'x'" }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text backfill "'y'" }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldBackfillChanged)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("backfill changed should be additive, got %+v", c)
	}
}

func TestDiff_SerialAdded_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  seq bigint }`)
	newIR := lower(t, `entity A in x { id bigint primary  seq bigint serial }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldSerialAdded)
	if c == nil || c.Class != ClassBackfillRequired || c.Field != "seq" {
		t.Errorf("SERIAL added should require backfill on seq, got %+v", c)
	}
}

func TestDiff_SerialRemoved_IsBackfill(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  seq bigint serial }`)
	newIR := lower(t, `entity A in x { id bigint primary  seq bigint }`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldSerialRemoved)
	if c == nil || c.Class != ClassBackfillRequired || c.Field != "seq" {
		t.Errorf("SERIAL removed should require backfill on seq, got %+v", c)
	}
}

func TestDiff_SerialUnchanged_NoSerialDiff(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  seq bigint serial }`)
	d := ComputeDiff(ir, ir)
	if c := findChange(t, d, KindFieldSerialAdded); c != nil {
		t.Errorf("identical IR should not produce SerialAdded, got %+v", c)
	}
	if c := findChange(t, d, KindFieldSerialRemoved); c != nil {
		t.Errorf("identical IR should not produce SerialRemoved, got %+v", c)
	}
}

func TestDiff_ReferenceAddedToExistingColumn_IsBackfill(t *testing.T) {
	oldIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint }
`)
	newIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint references x.Account.id }
`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindFieldReferenceAdded); c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("FK on existing column should require backfill, got %+v", c)
	}
}

func TestDiff_ReferenceAddedOnNewField_IsAdditive(t *testing.T) {
	oldIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary }
`)
	newIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint references x.Account.id }
`)
	d := ComputeDiff(oldIR, newIR)
	// New field, no existing rows → FK setup is purely additive.
	refCh := findChange(t, d, KindFieldReferenceAdded)
	if refCh == nil || refCh.Class != ClassAdditive {
		t.Errorf("FK on newly-added field should be additive, got %+v", refCh)
	}
}

func TestDiff_ReferenceRemoved_IsAdditive(t *testing.T) {
	oldIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint references x.Account.id }
`)
	newIR := lower(t, `
entity Account in x { id bigint primary }
entity B in x { id bigint primary  account_id bigint }
`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindFieldReferenceRemoved); c == nil || c.Class != ClassAdditive {
		t.Errorf("FK removal should be additive, got %+v", c)
	}
}

func TestDiff_ReferenceTargetChange_IsBreaking(t *testing.T) {
	// Pointing an FK at a different target table is a wire-visible behavior
	// change (callers may have integrity assumptions on the *target* row).
	// Classified breaking — see diff.go: FK action vs target distinction.
	oldIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary }
entity C in x { id bigint primary  ref bigint references x.A.id }
`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary }
entity C in x { id bigint primary  ref bigint references x.B.id }
`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindFieldReferenceModified); c == nil || c.Class != ClassCrossCallerBreaking {
		t.Errorf("FK target change should be breaking, got %+v", c)
	}
}

func TestDiff_ReferenceActionStrengthened_IsBackfill(t *testing.T) {
	oldIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary  ref bigint references x.A.id on delete cascade }
`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary  ref bigint references x.A.id on delete restrict }
`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldReferenceModified)
	if c == nil || c.Class != ClassBackfillRequired {
		t.Errorf("cascade→restrict should require backfill, got %+v", c)
	}
}

func TestDiff_ReferenceActionWeakened_IsAdditive(t *testing.T) {
	oldIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary  ref bigint references x.A.id on delete restrict }
`)
	newIR := lower(t, `
entity A in x { id bigint primary }
entity B in x { id bigint primary  ref bigint references x.A.id on delete cascade }
`)
	d := ComputeDiff(oldIR, newIR)
	c := findChange(t, d, KindFieldReferenceModified)
	if c == nil || c.Class != ClassAdditive {
		t.Errorf("restrict→cascade should be additive, got %+v", c)
	}
}

func TestDiff_IndexAdded(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v int }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int  index by v }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindIndexAdded); c == nil || c.Class != ClassAdditive {
		t.Errorf("index add should be additive, got %+v", c)
	}
}

func TestDiff_IndexRemoved(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v int  index by v }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindIndexRemoved); c == nil || c.Class != ClassAdditive {
		t.Errorf("index remove should be additive, got %+v", c)
	}
}

func TestDiff_IndexReorder_NoDiff(t *testing.T) {
	oldIR := lower(t, `
entity A in x {
  id bigint primary
  a int
  b int
  index by a
  index by b
}`)
	newIR := lower(t, `
entity A in x {
  id bigint primary
  a int
  b int
  index by b
  index by a
}`)
	if !ComputeDiff(oldIR, newIR).IsEmpty() {
		t.Errorf("index reorder should produce no diff")
	}
}

func TestDiff_CacheAdded(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=10m } }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindCacheChanged); c == nil || c.Class != ClassAdditive {
		t.Errorf("cache add should be additive, got %+v", c)
	}
}

func TestDiff_CacheTTLChanged(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=10m } }`)
	newIR := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=1h } }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindCacheChanged); c == nil || c.Class != ClassAdditive {
		t.Errorf("TTL change should be additive, got %+v", c)
	}
}

func TestDiff_QueryTimeoutChanged(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  query_timeout = 1s }`)
	newIR := lower(t, `entity A in x { id bigint primary  query_timeout = 30s }`)
	d := ComputeDiff(oldIR, newIR)
	if c := findChange(t, d, KindQueryTimeoutChanged); c == nil || c.Class != ClassAdditive {
		t.Errorf("query_timeout change should be additive, got %+v", c)
	}
}

func TestDiff_StableOrdering(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `
entity C in z { id bigint primary }
entity B in y { id bigint primary }
entity A in x { id bigint primary }
`)
	d := ComputeDiff(oldIR, newIR)
	if len(d.Additive) != 2 {
		t.Fatalf("want 2 additive, got %d", len(d.Additive))
	}
	// Should be sorted by ID: y.B then z.C.
	if d.Additive[0].EntityID != "y.B" || d.Additive[1].EntityID != "z.C" {
		t.Errorf("ordering: got %s, %s", d.Additive[0].EntityID, d.Additive[1].EntityID)
	}
}

func TestDiff_HighestClass_Mixed(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v int  added text }`)
	// v: text→int (backfill), added: new nullable (additive). Highest is backfill.
	d := ComputeDiff(oldIR, newIR)
	if d.HighestClass() != ClassBackfillRequired {
		t.Errorf("mixed diff should report backfill as highest, got %v", d.HighestClass())
	}
}

func TestDiff_FromNilToSomething(t *testing.T) {
	newIR := lower(t, `entity A in x { id bigint primary }`)
	d := ComputeDiff(nil, newIR)
	if c := findChange(t, d, KindEntityAdded); c == nil {
		t.Errorf("nil → IR should report entity added")
	}
}
