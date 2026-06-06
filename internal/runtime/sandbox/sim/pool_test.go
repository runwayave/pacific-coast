package sim_test

// The spike test: prove that the same SQL strings codegen emits for a
// representative entity round-trip through the sim Pool. We hand-build
// a TableDesc for an `account(id bigint pk, email text, active bool)`
// entity, then run INSERT / SELECT / UPDATE / DELETE in exactly the
// shapes internal/codegen/server.go produces.
//
// Tests wire real codegen-emitted SQL strings (captured from
// `tide generate` over a fixture IR) into the conformance harness; this
// file is the manual precursor that proves the seam itself works.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// makeAccountTable installs a descriptor mirroring what codegen would
// emit for an entity Account in namespace consumer with id (bigint PK),
// email (text not null), active (bool not null default true). Returns
// the pool ready to take statements.
func makeAccountTable(t *testing.T) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_account",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "email", Kind: sim.KindString},
			{Name: "active", Kind: sim.KindBool},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sim.NewPool(cat)
}

// TestInsertReturningRoundTrip mirrors the codegen-emitted Create path:
// INSERT ... RETURNING "id" is run via QueryRow so the handler can
// Scan the freshly assigned PK back out.
func TestInsertReturningRoundTrip(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()

	const sqlInsert = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	var id int64
	if err := pool.QueryRow(ctx, sqlInsert, int64(7), "a@b.com", true).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 7 {
		t.Fatalf("RETURNING id: got %d want 7", id)
	}

	const sqlGet = `SELECT "id", "email", "active" FROM "atlantis"."consumer_account" WHERE "id" = $1`
	var gotID int64
	var gotEmail string
	var gotActive bool
	if err := pool.QueryRow(ctx, sqlGet, int64(7)).Scan(&gotID, &gotEmail, &gotActive); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotID != 7 || gotEmail != "a@b.com" || !gotActive {
		t.Fatalf("get mismatch: id=%d email=%q active=%v", gotID, gotEmail, gotActive)
	}
}

// TestGetNoRowsSurfacesAtScan mirrors the pg adapter's no-rows path:
// QueryRow.Scan returns a no-rows error that runtime.IsNoRows detects.
func TestGetNoRowsSurfacesAtScan(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()
	const sqlGet = `SELECT "id", "email" FROM "atlantis"."consumer_account" WHERE "id" = $1`
	err := pool.QueryRow(ctx, sqlGet, int64(99)).Scan(new(int64), new(string))
	if err == nil {
		t.Fatalf("expected no-rows error, got nil")
	}
	if !runtime.IsNoRows(err) {
		t.Fatalf("runtime.IsNoRows missed sim no-rows error: %v", err)
	}
}

// TestUpdateRowsAffected exercises the path generated UPDATE handlers
// use to detect "row not found": the handler reads tag.RowsAffected()
// and translates 0 into runtime.ErrNotFound. We assert both branches.
func TestUpdateRowsAffected(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()

	const sqlInsert = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	if err := pool.QueryRow(ctx, sqlInsert, int64(1), "x@y.com", true).Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const sqlUpdate = `UPDATE "atlantis"."consumer_account" SET "email" = $1, "active" = $2 WHERE "id" = $3`

	// Existing row -> 1
	tag, err := pool.Exec(ctx, sqlUpdate, "z@y.com", false, int64(1))
	if err != nil {
		t.Fatalf("update existing: %v", err)
	}
	if got := tag.RowsAffected(); got != 1 {
		t.Fatalf("existing RowsAffected: got %d want 1", got)
	}

	// Confirm UPDATE actually wrote.
	const sqlGet = `SELECT "email", "active" FROM "atlantis"."consumer_account" WHERE "id" = $1`
	var email string
	var active bool
	if err := pool.QueryRow(ctx, sqlGet, int64(1)).Scan(&email, &active); err != nil {
		t.Fatalf("post-update get: %v", err)
	}
	if email != "z@y.com" || active {
		t.Fatalf("update did not write: email=%q active=%v", email, active)
	}

	// Missing row -> 0
	tag, err = pool.Exec(ctx, sqlUpdate, "n@y.com", true, int64(9999))
	if err != nil {
		t.Fatalf("update missing: %v", err)
	}
	if got := tag.RowsAffected(); got != 0 {
		t.Fatalf("missing RowsAffected: got %d want 0", got)
	}
}

// TestDeleteRowsAffected is the symmetric DELETE counterpart.
func TestDeleteRowsAffected(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()

	const sqlInsert = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	if err := pool.QueryRow(ctx, sqlInsert, int64(42), "k@v.com", true).Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const sqlDelete = `DELETE FROM "atlantis"."consumer_account" WHERE "id" = $1`

	tag, err := pool.Exec(ctx, sqlDelete, int64(42))
	if err != nil {
		t.Fatalf("delete existing: %v", err)
	}
	if got := tag.RowsAffected(); got != 1 {
		t.Fatalf("existing RowsAffected: got %d want 1", got)
	}

	tag, err = pool.Exec(ctx, sqlDelete, int64(42))
	if err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if got := tag.RowsAffected(); got != 0 {
		t.Fatalf("missing RowsAffected: got %d want 0", got)
	}
}

// TestPKConflict asserts that INSERTing a row with a colliding PK
// surfaces as an error (which generated Create handlers map to
// codes.AlreadyExists at the RPC layer).
func TestPKConflict(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()
	const sqlInsert = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	if err := pool.QueryRow(ctx, sqlInsert, int64(1), "a@b.com", true).Scan(new(int64)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := pool.QueryRow(ctx, sqlInsert, int64(1), "c@d.com", false).Scan(new(int64))
	if err == nil {
		t.Fatalf("expected PK conflict, got nil")
	}
	if !errors.Is(err, sim.ErrPKConflict) {
		t.Fatalf("expected ErrPKConflict, got %v", err)
	}
}

// TestUnsupportedSurfacesErrUnsupported asserts the whitelist boundary:
// constructs the executor doesn't model surface as ErrUnsupported with
// a helpful snippet, not a parse-time crash or a silently-wrong result.
// GROUP BY is the load-bearing example here — the parser accepts it
// (pg_query handles full PG grammar), the translator rejects it
// up-front because the executor doesn't aggregate.
func TestUnsupportedSurfacesErrUnsupported(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()
	_, err := pool.Query(ctx, `SELECT "id" FROM "atlantis"."consumer_account" GROUP BY "id"`)
	if err == nil {
		t.Fatalf("expected unsupported error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported in message, got %v", err)
	}
}

// TestTxCommitCarriesWrites confirms that writes made inside a Tx are
// visible to subsequent reads through Pool. Tx today is a thin
// passthrough; the test pins the contract a future CoW-rollback
// snapshots replace it.
func TestTxCommitCarriesWrites(t *testing.T) {
	pool := makeAccountTable(t)
	ctx := context.Background()

	tx, err := pool.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	const sqlInsert = `INSERT INTO "atlantis"."consumer_account" ("id", "email", "active") VALUES ($1, $2, $3) RETURNING "id"`
	if err := tx.QueryRow(ctx, sqlInsert, int64(11), "tx@y.com", true).Scan(new(int64)); err != nil {
		t.Fatalf("tx insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	const sqlGet = `SELECT "email" FROM "atlantis"."consumer_account" WHERE "id" = $1`
	var email string
	if err := pool.QueryRow(ctx, sqlGet, int64(11)).Scan(&email); err != nil {
		t.Fatalf("post-commit get: %v", err)
	}
	if email != "tx@y.com" {
		t.Fatalf("post-commit email: got %q want tx@y.com", email)
	}
}
