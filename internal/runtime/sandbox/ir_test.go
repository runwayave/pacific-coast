package sandbox_test

// IR-translation tests. Build a minimal *dsl.IR by hand (avoiding the
// need to read .atl files in tests), pass it through sandbox.New, and
// assert the catalog comes out the way generated codegen-emitted SQL
// would expect.

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// makeAccountIR builds a tiny IR shaped like a real consumer.Account
// entity: bigint identity PK, text email, varchar(20) plan with a
// 'pending' default, timestamptz deleted_at for soft-delete.
func makeAccountIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Account",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "plan", Type: dsl.FieldType{Name: "varchar", Len: 20}, NotNull: true,
					Default: &dsl.Default{Kind: dsl.DefaultIRString, Str: "pending"}},
				{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}},
			},
			SoftDeleteField: "deleted_at",
		}},
	}
}

// TestLoadIRBuildsCatalog confirms the IR translator produces the
// schema/name/PK/metadata the codegen-emitted SQL expects.
func TestLoadIRBuildsCatalog(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: makeAccountIR()})

	desc := sb.Catalog().Lookup("atlantis.consumer_account")
	if desc == nil {
		t.Fatalf("entity Account not registered as atlantis.consumer_account")
	}
	if len(desc.PKCols) != 1 || desc.PKCols[0] != "id" {
		t.Fatalf("PK: %v", desc.PKCols)
	}
	if desc.SoftDeleteField != "deleted_at" {
		t.Fatalf("SoftDeleteField: %q", desc.SoftDeleteField)
	}
	if desc.IdentityCol != "id" {
		t.Fatalf("IdentityCol: %q", desc.IdentityCol)
	}
	// 4 declared fields → 4 columns.
	if len(desc.Cols) != 4 {
		t.Fatalf("cols: %d (%v)", len(desc.Cols), desc.Cols)
	}
}

// TestLoadIRThenInsertRoundTrip exercises the full path: IR → catalog
// → INSERT via the simulated SQL surface → SELECT roundtrips.
func TestLoadIRThenInsertRoundTrip(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: makeAccountIR()})
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "plan", "deleted_at") VALUES ($1, $2, $3, $4) RETURNING "id"`
	var id int64
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(1), "x@y.com", "trial", nil).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 1 {
		t.Fatalf("RETURNING: got %d want 1", id)
	}

	const sqlGet = `SELECT "email", "plan" FROM "atlantis"."consumer_account" WHERE "id" = $1 AND "deleted_at" IS NULL`
	var email, plan string
	if err := sb.Pool().QueryRow(ctx, sqlGet, int64(1)).Scan(&email, &plan); err != nil {
		t.Fatalf("get: %v", err)
	}
	if email != "x@y.com" || plan != "trial" {
		t.Fatalf("get: email=%q plan=%q", email, plan)
	}
}

// TestLoadIRCompositePKAndHypertable confirms hypertable+composite-PK
// entities (the schema shape the IR loader needs to cover for the Purchase
// hypertable referenced by real caller schemas) translate correctly.
func TestLoadIRCompositePKAndHypertable(t *testing.T) {
	ir := &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Purchase",
			Namespace: "vendor",
			Kind:      dsl.EntityKindHypertable,
			TimeField: "occurred_at",
			Fields: []dsl.Field{
				{Name: "purchase_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
				{Name: "occurred_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
				{Name: "amount_cents", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			},
			CompositePK: []string{"purchase_id", "occurred_at"},
		}},
	}
	sb := sandbox.NewT(t, sandbox.Options{IR: ir})
	desc := sb.Catalog().Lookup("atlantis.vendor_purchase")
	if desc == nil {
		t.Fatalf("Purchase not registered")
	}
	if len(desc.PKCols) != 2 || desc.PKCols[0] != "purchase_id" || desc.PKCols[1] != "occurred_at" {
		t.Fatalf("composite PK: %v", desc.PKCols)
	}
	if desc.TimeField != "occurred_at" {
		t.Fatalf("TimeField captured for hypertable: %q", desc.TimeField)
	}
}

// TestLoadIRTableNameOverride exercises the `table "schema.name"`
// override — used when adopting existing prod tables.
func TestLoadIRTableNameOverride(t *testing.T) {
	ir := &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Legacy",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			TableName: "public.legacy_users",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
			},
		}},
	}
	sb := sandbox.NewT(t, sandbox.Options{IR: ir})
	if sb.Catalog().Lookup("public.legacy_users") == nil {
		t.Fatalf("TableName override not honored")
	}
	if sb.Catalog().Lookup("atlantis.consumer_legacy") != nil {
		t.Fatalf("default name still registered alongside override")
	}
}

// TestLoadIRAcceptsArrayColumns pins the post-bootstrap behaviour:
// array columns load as KindArray (opaque storage) so real caller
// schemas with text[] / int[] fields don't block sandbox boot. PG
// array operators stay unsupported and error at query time.
func TestLoadIRAcceptsArrayColumns(t *testing.T) {
	ir := &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Tagged",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				{Name: "tags", Type: dsl.FieldType{Name: "text", Array: true}},
			},
		}},
	}
	sb, err := sandbox.New(sandbox.Options{IR: ir})
	if err != nil {
		t.Fatalf("unexpected error registering array entity: %v", err)
	}
	defer sb.Close()
	desc := sb.Catalog().Lookup("atlantis.consumer_tagged")
	if desc == nil {
		t.Fatalf("Tagged entity not registered")
	}
}

// TestSnapshotRoundTrip exercises Snapshot → mutate → Restore: the
// state goes back to what it was at Snapshot time.
func TestSnapshotRoundTrip(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: makeAccountIR()})
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "plan", "deleted_at") VALUES ($1, $2, $3, $4) RETURNING "id"`
	for i, email := range []string{"a@b.com", "c@d.com"} {
		if err := sb.Pool().QueryRow(ctx, sqlIns, int64(i+1), email, "pro", nil).Scan(new(int64)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	blob, err := sb.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(blob) == 0 {
		t.Fatalf("snapshot returned empty blob")
	}

	// Mutate: add a third row + soft-delete row 1.
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(3), "e@f.com", "pro", nil).Scan(new(int64)); err != nil {
		t.Fatalf("post-snapshot insert: %v", err)
	}
	const sqlSoftDel = `UPDATE "atlantis"."consumer_account" SET "deleted_at" = now() WHERE "id" = $1 AND "deleted_at" IS NULL`
	if _, err := sb.Pool().Exec(ctx, sqlSoftDel, int64(1)); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Restore: state goes back. Row 1 is undeleted, row 3 is gone.
	if err := sb.Restore(blob); err != nil {
		t.Fatalf("restore: %v", err)
	}

	const sqlGetAll = `SELECT "id", "email" FROM "atlantis"."consumer_account" WHERE "deleted_at" IS NULL ORDER BY "id" ASC`
	rows, err := sb.Pool().Query(ctx, sqlGetAll)
	if err != nil {
		t.Fatalf("post-restore list: %v", err)
	}
	defer rows.Close()
	type r struct {
		ID    int64
		Email string
	}
	var got []r
	for rows.Next() {
		var x r
		if err := rows.Scan(&x.ID, &x.Email); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, x)
	}
	if len(got) != 2 {
		t.Fatalf("post-restore rows: got %d want 2 (%+v)", len(got), got)
	}
	if got[0].ID != 1 || got[0].Email != "a@b.com" || got[1].ID != 2 || got[1].Email != "c@d.com" {
		t.Fatalf("post-restore content: %+v", got)
	}
}

// TestSnapshotSchemaMismatch confirms the safety contract: loading a
// blob taken under schema A into schema B refuses with the typed
// error rather than silently corrupting state.
func TestSnapshotSchemaMismatch(t *testing.T) {
	// Snapshot under schema A.
	sbA := sandbox.NewT(t, sandbox.Options{IR: makeAccountIR()})
	blob, err := sbA.Snapshot()
	if err != nil {
		t.Fatalf("snapshot A: %v", err)
	}

	// Schema B: same entity but a different field set.
	irB := &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "Account",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
				// Renamed "email" → "user_email" — schema mismatch.
				{Name: "user_email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}},
	}
	sbB := sandbox.NewT(t, sandbox.Options{IR: irB})
	if err := sbB.Restore(blob); !errors.Is(err, sandbox.ErrSnapshotSchemaMismatch) {
		t.Fatalf("expected ErrSnapshotSchemaMismatch, got %v", err)
	}
}

// TestSnapshotFormatErrors confirms unrecognised blob payloads fail
// with the format sentinel rather than a corrupt-state restore.
func TestSnapshotFormatErrors(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: makeAccountIR()})
	if err := sb.Restore([]byte("garbage")); err == nil {
		t.Fatalf("expected error for garbage blob")
	}
}
