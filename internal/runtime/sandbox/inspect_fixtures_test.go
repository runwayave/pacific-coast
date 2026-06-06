package sandbox_test

// Tests for the two remaining category-defining features: the
// Inspect API (LLM tool-use surface) and the Fixtures runtime
// (synthetic-from-schema seed).

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// peopleIR: bigint id PK + string email + bool active + nullable
// numeric balance. Covers enough kinds that fixtures and inspect
// exercise their per-kind branches.
func peopleIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Person",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "active", Type: dsl.FieldType{Name: "boolean"}, NotNull: true},
				{Name: "balance", Type: dsl.FieldType{Name: "numeric"}},
			},
			SoftDeleteField: "",
		}},
	}
}

// ─────────────────────────── Inspect ───────────────────────────

func TestInspectDescribe(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})

	d, err := sb.Inspect().Describe("atlantis.consumer_person")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.Schema != "atlantis" || d.Name != "consumer_person" {
		t.Fatalf("schema/name: %+v", d)
	}
	if len(d.Columns) != 4 {
		t.Fatalf("columns: got %d want 4", len(d.Columns))
	}
	if d.PrimaryKey[0] != "id" {
		t.Fatalf("PK: %v", d.PrimaryKey)
	}
	if d.IdentityCol != "id" {
		t.Fatalf("IdentityCol: %q", d.IdentityCol)
	}
	if d.RowCount != 0 {
		t.Fatalf("RowCount on empty table: got %d want 0", d.RowCount)
	}
}

func TestInspectDescribeUnknownEntity(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})
	_, err := sb.Inspect().Describe("atlantis.bogus")
	if !errors.Is(err, sandbox.ErrUnknownEntity) {
		t.Fatalf("expected ErrUnknownEntity, got %v", err)
	}
}

func TestInspectSampleAndFind(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."consumer_person" ("id", "email", "active", "balance") VALUES ($1, $2, $3, $4) RETURNING "id"`
	for i, e := range []string{"a@y.com", "b@y.com", "c@y.com"} {
		active := i%2 == 0
		if err := sb.Pool().QueryRow(ctx, sqlIns, int64(i+1), e, active, nil).Scan(new(int64)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Sample
	rows, err := sb.Inspect().Sample("atlantis.consumer_person", 2)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("sample: got %d want 2", len(rows))
	}
	if rows[0]["email"] != "a@y.com" {
		t.Fatalf("sample[0].email: %v", rows[0]["email"])
	}

	// Find
	matches, err := sb.Inspect().Find("atlantis.consumer_person", sandbox.Predicate{
		Column: "active", Op: sandbox.PredEq, Value: true,
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	// id 1 (active=true) and id 3 (active=true)
	if len(matches) != 2 {
		t.Fatalf("find active=true: got %d want 2 (%v)", len(matches), matches)
	}

	// Find with NOT NULL
	withBalance, err := sb.Inspect().Find("atlantis.consumer_person", sandbox.Predicate{
		Column: "balance", Op: sandbox.PredNotNull,
	})
	if err != nil {
		t.Fatalf("find balance not null: %v", err)
	}
	if len(withBalance) != 0 {
		t.Fatalf("find balance not null: got %d want 0", len(withBalance))
	}
}

func TestInspectFindUnknownColumn(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})
	_, err := sb.Inspect().Find("atlantis.consumer_person", sandbox.Predicate{
		Column: "bogus", Op: sandbox.PredEq, Value: 1,
	})
	if !errors.Is(err, sandbox.ErrUnknownColumn) {
		t.Fatalf("expected ErrUnknownColumn, got %v", err)
	}
}

func TestInspectDiff(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."consumer_person" ("id", "email", "active", "balance") VALUES ($1, $2, $3, $4) RETURNING "id"`
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(1), "a@y.com", true, nil).Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(2), "b@y.com", true, nil).Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m1 := sb.Mark()

	// Mutate: add row 3, change row 1's email, delete row 2.
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(3), "c@y.com", false, nil).Scan(new(int64)); err != nil {
		t.Fatalf("add 3: %v", err)
	}
	const sqlUpd = `UPDATE "atlantis"."consumer_person" SET "email" = $1 WHERE "id" = $2`
	if _, err := sb.Pool().Exec(ctx, sqlUpd, "changed@y.com", int64(1)); err != nil {
		t.Fatalf("upd 1: %v", err)
	}
	const sqlDel = `DELETE FROM "atlantis"."consumer_person" WHERE "id" = $1`
	if _, err := sb.Pool().Exec(ctx, sqlDel, int64(2)); err != nil {
		t.Fatalf("del 2: %v", err)
	}

	m2 := sb.Mark()

	diff, err := sb.Inspect().Diff(m1, m2)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	td, ok := diff.Tables["atlantis.consumer_person"]
	if !ok {
		t.Fatalf("Person not in diff: %+v", diff)
	}
	if td.Added != 1 || td.Removed != 1 || td.Modified != 1 {
		t.Fatalf("diff: %+v want Added=1 Removed=1 Modified=1", td)
	}
}

// ─────────────────────────── Fixtures ───────────────────────────

func TestFixturesBulkBasic(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR(), Seed: 7})
	ctx := context.Background()

	n, err := sb.Fixtures().Bulk(ctx, "atlantis.consumer_person", 10, sandbox.BulkOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 10 {
		t.Fatalf("bulk: got %d want 10", n)
	}

	d, err := sb.Inspect().Describe("atlantis.consumer_person")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if d.RowCount != 10 {
		t.Fatalf("post-bulk row count: %d want 10", d.RowCount)
	}

	// Every row's email matches the seeded shape.
	rows, err := sb.Inspect().Sample("atlantis.consumer_person", 3)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	for _, r := range rows {
		em, ok := r["email"].(string)
		if !ok || em == "" {
			t.Fatalf("row email: %v (%T)", r["email"], r["email"])
		}
	}
}

func TestFixturesBulkReproducible(t *testing.T) {
	// Two independent sandboxes seeded identically produce identical
	// row sets — the bedrock of agent-test reproducibility.
	mkSB := func() *sandbox.Sandbox {
		return sandbox.NewT(t, sandbox.Options{IR: peopleIR(), Seed: 99})
	}
	sbA := mkSB()
	sbB := mkSB()
	ctx := context.Background()
	if _, err := sbA.Fixtures().Bulk(ctx, "atlantis.consumer_person", 5, sandbox.BulkOptions{}); err != nil {
		t.Fatalf("A bulk: %v", err)
	}
	if _, err := sbB.Fixtures().Bulk(ctx, "atlantis.consumer_person", 5, sandbox.BulkOptions{}); err != nil {
		t.Fatalf("B bulk: %v", err)
	}
	rowsA, _ := sbA.Inspect().Sample("atlantis.consumer_person", 5)
	rowsB, _ := sbB.Inspect().Sample("atlantis.consumer_person", 5)
	if len(rowsA) != len(rowsB) {
		t.Fatalf("row counts differ: %d vs %d", len(rowsA), len(rowsB))
	}
	for i := range rowsA {
		if rowsA[i]["email"] != rowsB[i]["email"] {
			t.Fatalf("row %d email diverges: %v vs %v",
				i, rowsA[i]["email"], rowsB[i]["email"])
		}
		if rowsA[i]["id"] != rowsB[i]["id"] {
			t.Fatalf("row %d id diverges: %v vs %v",
				i, rowsA[i]["id"], rowsB[i]["id"])
		}
	}
}

func TestFixturesBulkUnknownEntity(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR()})
	_, err := sb.Fixtures().Bulk(context.Background(), "atlantis.bogus", 5, sandbox.BulkOptions{})
	if !errors.Is(err, sandbox.ErrUnknownEntity) {
		t.Fatalf("expected ErrUnknownEntity, got %v", err)
	}
}

func TestFixturesBulkPKStart(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: peopleIR(), Seed: 1})
	ctx := context.Background()

	if _, err := sb.Fixtures().Bulk(ctx, "atlantis.consumer_person", 3, sandbox.BulkOptions{
		PKStart: 1000,
	}); err != nil {
		t.Fatalf("bulk: %v", err)
	}
	rows, err := sb.Inspect().Sample("atlantis.consumer_person", 3)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	// PK should start at 1000 and walk monotonically.
	if rows[0]["id"].(int64) != 1000 || rows[1]["id"].(int64) != 1001 || rows[2]["id"].(int64) != 1002 {
		t.Fatalf("PK sequence: %v %v %v", rows[0]["id"], rows[1]["id"], rows[2]["id"])
	}
}
