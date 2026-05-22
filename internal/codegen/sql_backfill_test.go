package codegen

import (
	"strings"
	"testing"
)

func TestEmitSQL_NoBackfill_PhaseScriptsEmpty(t *testing.T) {
	oldIR := lower(t, `entity A in x { id bigint primary }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if scripts.PreBackfillUp != "" {
		t.Errorf("non-backfill plan should leave PreBackfillUp empty, got %q", scripts.PreBackfillUp)
	}
	if scripts.PostBackfillUp != "" {
		t.Errorf("non-backfill plan should leave PostBackfillUp empty, got %q", scripts.PostBackfillUp)
	}
	if scripts.PreBackfillIndexes != "" {
		t.Errorf("non-backfill plan should leave PreBackfillIndexes empty, got %q", scripts.PreBackfillIndexes)
	}
	if scripts.PostBackfillIndexes != "" {
		t.Errorf("non-backfill plan should leave PostBackfillIndexes empty, got %q", scripts.PostBackfillIndexes)
	}
}

func TestEmitSQL_NewNotNullFieldWithBackfill_PhaseSplit(t *testing.T) {
	oldIR := lower(t, `
entity User in consumer {
  id         bigint primary
  first_name text not null
  last_name  text not null
}
`)
	newIR := lower(t, `
entity User in consumer {
  id           bigint primary
  first_name   text not null
  last_name    text not null
  display_name text not null backfill "first_name || ' ' || last_name"
}
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// PreBackfillUp should ADD COLUMN as nullable (no NOT NULL inline).
	if !strings.Contains(scripts.PreBackfillUp, "ADD COLUMN") {
		t.Errorf("PreBackfillUp should contain ADD COLUMN, got:\n%s", scripts.PreBackfillUp)
	}
	// The added column's line should NOT have NOT NULL — the nullable form.
	addLine := findLineContaining(scripts.PreBackfillUp, "ADD COLUMN")
	if strings.Contains(addLine, "NOT NULL") {
		t.Errorf("PreBackfillUp's ADD COLUMN must drop NOT NULL for backfill; got: %s", addLine)
	}

	// PostBackfillUp should contain SET NOT NULL on display_name.
	if !strings.Contains(scripts.PostBackfillUp, "SET NOT NULL") {
		t.Errorf("PostBackfillUp should contain SET NOT NULL, got:\n%s", scripts.PostBackfillUp)
	}
	if !strings.Contains(scripts.PostBackfillUp, `"display_name"`) {
		t.Errorf("PostBackfillUp should target display_name, got:\n%s", scripts.PostBackfillUp)
	}

	// PreBackfillIndexes should CREATE INDEX CONCURRENTLY on the NULL set.
	if !strings.Contains(scripts.PreBackfillIndexes, "CREATE INDEX CONCURRENTLY") {
		t.Errorf("PreBackfillIndexes missing CREATE INDEX CONCURRENTLY:\n%s", scripts.PreBackfillIndexes)
	}
	if !strings.Contains(scripts.PreBackfillIndexes, `"display_name" IS NULL`) {
		t.Errorf("partial index should be on display_name IS NULL:\n%s", scripts.PreBackfillIndexes)
	}

	// PostBackfillIndexes should DROP INDEX CONCURRENTLY.
	if !strings.Contains(scripts.PostBackfillIndexes, "DROP INDEX CONCURRENTLY") {
		t.Errorf("PostBackfillIndexes missing DROP INDEX CONCURRENTLY:\n%s", scripts.PostBackfillIndexes)
	}
}

func TestEmitSQL_NotNullTightenedWithBackfill_PhaseSplit(t *testing.T) {
	// Field already exists nullable. New schema tightens to NOT NULL with backfill.
	oldIR := lower(t, `
entity A in x {
  id         bigint primary
  v          text
}
`)
	newIR := lower(t, `
entity A in x {
  id         bigint primary
  v          text not null backfill "'default'"
}
`)
	d := ComputeDiff(oldIR, newIR)
	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// SET NOT NULL must be deferred to PostBackfillUp.
	if !strings.Contains(scripts.PostBackfillUp, "SET NOT NULL") {
		t.Errorf("PostBackfillUp should contain SET NOT NULL on v, got:\n%s", scripts.PostBackfillUp)
	}

	// PreBackfillUp must NOT contain SET NOT NULL on v.
	for _, line := range strings.Split(scripts.PreBackfillUp, "\n") {
		if strings.Contains(line, "SET NOT NULL") && strings.Contains(line, `"v"`) {
			t.Errorf("PreBackfillUp must defer SET NOT NULL on v to post, got line: %s", line)
		}
	}

	// Index lifecycle present.
	if !strings.Contains(scripts.PreBackfillIndexes, "CREATE INDEX CONCURRENTLY") {
		t.Errorf("PreBackfillIndexes missing index create:\n%s", scripts.PreBackfillIndexes)
	}
}

func TestEmitSQL_BackfillOnNullableField_NoPhaseSplit(t *testing.T) {
	// Adding the backfill modifier without NOT NULL — there's nothing the
	// SQL emitter needs to defer, so no phase split.
	oldIR := lower(t, `entity A in x { id bigint primary  v text }`)
	newIR := lower(t, `entity A in x { id bigint primary  v text backfill "'x'" }`)
	d := ComputeDiff(oldIR, newIR)
	scripts, err := EmitSQL(oldIR, newIR, d)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if scripts.PreBackfillUp != "" || scripts.PostBackfillUp != "" {
		t.Errorf("backfill on nullable field should not trigger phase split; pre=%q post=%q",
			scripts.PreBackfillUp, scripts.PostBackfillUp)
	}
}

func findLineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
