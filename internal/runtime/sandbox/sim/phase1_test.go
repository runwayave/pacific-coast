package sim_test

// Phase 1 surface tests. Phase 0's tests in pool_test.go still pin the
// Phase 0 contract (CRUD round-trip, no-rows-at-scan, RowsAffected,
// PK conflict, Tx commit visibility); this file adds coverage for the
// expanded codegen surface: ANY($1), ORDER BY, LIMIT/OFFSET, COUNT(*)
// OVER, IS NULL, COALESCE defaults, now() inside SET, composite PKs.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
	simsql "github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim/sql"
)

// Helper: a fixed clock so now()-using SQL is deterministic in tests.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// makeUserTable installs an entity with one nullable timestamp field
// representing soft-delete metadata. The shape mirrors what codegen
// emits for an entity with `soft_delete by deleted_at`.
func makeUserTable(t *testing.T, clock func() time.Time) *sim.Pool {
	t.Helper()
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_user",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "email", Kind: sim.KindString},
			{Name: "plan", Kind: sim.KindString},
			{Name: "deleted_at", Kind: sim.KindString, Nullable: true},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	opts := sim.PoolOptions{}
	if clock != nil {
		opts.Clock = clock
	}
	return sim.NewPoolWithOptions(cat, opts)
}

func seedUsers(t *testing.T, pool *sim.Pool, n int) {
	t.Helper()
	const sqlIns = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan", "deleted_at") VALUES ($1, $2, $3, $4) RETURNING "id"`
	for i := 1; i <= n; i++ {
		if err := pool.QueryRow(context.Background(), sqlIns,
			int64(i),
			pad("u", i),
			"pro",
			nil,
		).Scan(new(int64)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func pad(prefix string, n int) string {
	// Deterministic email-shaped strings so ORDER BY assertions are stable.
	return prefix + itoa(n) + "@example.com"
}

func itoa(n int) string {
	// Tiny hand-rolled itoa so the test file doesn't need strconv just
	// to format two-digit IDs.
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ────────────────────────── tests ──────────────────────────

// TestBatchGetANY exercises `WHERE pk = ANY($1)` — the BatchGet path
// codegen emits when the handler receives a list of IDs.
func TestBatchGetANY(t *testing.T) {
	pool := makeUserTable(t, nil)
	seedUsers(t, pool, 5)
	ctx := context.Background()

	const sqlBatch = `SELECT "id", "email" FROM "atlantis"."consumer_user" WHERE "id" = ANY($1)`
	rows, err := pool.Query(ctx, sqlBatch, []int64{1, 3, 5})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	want := map[int64]string{1: "u1@example.com", 3: "u3@example.com", 5: "u5@example.com"}
	seen := map[int64]string{}
	for rows.Next() {
		var id int64
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[id] = email
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("rows: got %d want %d (%v)", len(seen), len(want), seen)
	}
	for id, email := range want {
		if seen[id] != email {
			t.Fatalf("id=%d: got %q want %q", id, seen[id], email)
		}
	}
}

// TestOrderByAscThenLimit exercises ORDER BY id ASC + LIMIT + OFFSET —
// the basic List-without-keyset path codegen produces.
func TestOrderByAscThenLimit(t *testing.T) {
	pool := makeUserTable(t, nil)
	seedUsers(t, pool, 10)
	ctx := context.Background()

	const sqlList = `SELECT "id" FROM "atlantis"."consumer_user" ORDER BY "id" ASC LIMIT $1 OFFSET $2`
	rows, err := pool.Query(ctx, sqlList, int64(3), int64(2))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	want := []int64{3, 4, 5}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("rows: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows[%d]: got %d want %d", i, got[i], want[i])
		}
	}
}

// TestOrderByDescNullsFirst confirms PG's default NULL ordering: DESC
// puts NULLs first. We seed three rows with id+plan, set plan=NULL on
// one, and assert it sorts first under DESC.
func TestOrderByDescNullsFirst(t *testing.T) {
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "vendor_account",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "rank", Kind: sim.KindInt64, Nullable: true},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	pool := sim.NewPool(cat)
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."vendor_account" ("id", "rank") VALUES ($1, $2) RETURNING "id"`
	for _, p := range []struct {
		id   int64
		rank any
	}{
		{1, int64(10)},
		{2, nil}, // NULL — should sort first under DESC
		{3, int64(20)},
	} {
		if err := pool.QueryRow(ctx, sqlIns, p.id, p.rank).Scan(new(int64)); err != nil {
			t.Fatalf("seed %d: %v", p.id, err)
		}
	}

	const sqlList = `SELECT "id" FROM "atlantis"."vendor_account" ORDER BY "rank" DESC`
	rows, err := pool.Query(ctx, sqlList)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	want := []int64{2, 3, 1}
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("rows: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows[%d]: got %d want %d (full=%v)", i, got[i], want[i], got)
		}
	}
}

// TestCountOverTotal exercises `COUNT(*) OVER () AS total` projected
// alongside paged rows. The total reflects the pre-LIMIT row count.
func TestCountOverTotal(t *testing.T) {
	pool := makeUserTable(t, nil)
	seedUsers(t, pool, 7)
	ctx := context.Background()

	const sqlList = `SELECT "id", "email", COUNT(*) OVER () AS total FROM "atlantis"."consumer_user" ORDER BY "id" ASC LIMIT $1 OFFSET $2`
	rows, err := pool.Query(ctx, sqlList, int64(2), int64(0))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	wantTotal := int64(7)
	wantIDs := []int64{1, 2}
	var gotIDs []int64
	for rows.Next() {
		var id int64
		var email string
		var total int64
		if err := rows.Scan(&id, &email, &total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if total != wantTotal {
			t.Fatalf("COUNT(*) OVER: got %d want %d", total, wantTotal)
		}
		gotIDs = append(gotIDs, id)
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("rows: got %d want %d", len(gotIDs), len(wantIDs))
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("rows[%d]: got %d want %d", i, gotIDs[i], wantIDs[i])
		}
	}
}

// TestSoftDeleteIsNullFilter exercises soft-delete: reads append
// `AND deleted_at IS NULL` to exclude rows that have been soft-deleted.
func TestSoftDeleteIsNullFilter(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	pool := makeUserTable(t, fixedClock(now))
	seedUsers(t, pool, 3)
	ctx := context.Background()

	// Soft-delete user 2 via UPDATE SET deleted_at = now().
	const sqlSoftDel = `UPDATE "atlantis"."consumer_user" SET "deleted_at" = now() WHERE "id" = $1 AND "deleted_at" IS NULL`
	tag, err := pool.Exec(ctx, sqlSoftDel, int64(2))
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("soft delete RowsAffected: got %d want 1", tag.RowsAffected())
	}

	// Get with the soft-delete filter: user 2 should now be invisible.
	const sqlGet = `SELECT "id" FROM "atlantis"."consumer_user" WHERE "id" = $1 AND "deleted_at" IS NULL`
	if err := pool.QueryRow(ctx, sqlGet, int64(2)).Scan(new(int64)); err == nil {
		t.Fatalf("expected no-rows after soft-delete; got success")
	}

	// List with soft-delete filter: should see 2 rows (1 and 3).
	const sqlList = `SELECT "id" FROM "atlantis"."consumer_user" WHERE "deleted_at" IS NULL ORDER BY "id" ASC`
	rows, err := pool.Query(ctx, sqlList)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	want := []int64{1, 3}
	if len(ids) != len(want) || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("list: got %v want %v", ids, want)
	}
}

// TestInsertCoalesceDefault exercises COALESCE($N::TYPE, default_expr)
// — the codegen-emitted INSERT pattern for columns with DEFAULT. When
// the caller passes nil for the placeholder, the default expression
// (a literal or now()) supplies the value.
func TestInsertCoalesceDefault(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	pool := makeUserTable(t, fixedClock(now))
	ctx := context.Background()

	// COALESCE($N::TIMESTAMPTZ, now()): nil passes → now() value used.
	const sqlIns = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan", "deleted_at") VALUES ($1, $2, COALESCE($3::VARCHAR(20), 'pending'), $4) RETURNING "id"`
	if err := pool.QueryRow(ctx, sqlIns, int64(42), "x@y.com", nil, nil).Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const sqlGet = `SELECT "plan" FROM "atlantis"."consumer_user" WHERE "id" = $1`
	var plan string
	if err := pool.QueryRow(ctx, sqlGet, int64(42)).Scan(&plan); err != nil {
		t.Fatalf("get: %v", err)
	}
	if plan != "pending" {
		t.Fatalf("COALESCE default: got %q want %q", plan, "pending")
	}

	// Same statement, but caller supplied a non-nil value: COALESCE
	// should return that value, not the default.
	if err := pool.QueryRow(ctx, sqlIns, int64(43), "z@y.com", "trial", nil).Scan(new(int64)); err != nil {
		t.Fatalf("insert with value: %v", err)
	}
	if err := pool.QueryRow(ctx, sqlGet, int64(43)).Scan(&plan); err != nil {
		t.Fatalf("get: %v", err)
	}
	if plan != "trial" {
		t.Fatalf("COALESCE value: got %q want %q", plan, "trial")
	}
}

// TestNowInSet exercises `SET col = now()` — the codegen-emitted soft-
// delete UPDATE form. The clock injection produces a deterministic
// timestamp string in the column.
func TestNowInSet(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	pool := makeUserTable(t, fixedClock(now))
	seedUsers(t, pool, 1)
	ctx := context.Background()

	const sqlSet = `UPDATE "atlantis"."consumer_user" SET "deleted_at" = now() WHERE "id" = $1`
	if _, err := pool.Exec(ctx, sqlSet, int64(1)); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The simulator stores time.Time in the column. Scan into *time.Time
	// to inspect.
	const sqlGet = `SELECT "deleted_at" FROM "atlantis"."consumer_user" WHERE "id" = $1`
	var got time.Time
	if err := pool.QueryRow(ctx, sqlGet, int64(1)).Scan(&got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Equal(now) {
		t.Fatalf("now() in SET: got %v want %v", got, now)
	}
}

// TestUnsupportedFunctionSurfacesError pins the executor's loud
// rejection contract for unknown function names.
func TestUnsupportedFunctionSurfacesError(t *testing.T) {
	pool := makeUserTable(t, nil)
	ctx := context.Background()
	const sqlBad = `UPDATE "atlantis"."consumer_user" SET "email" = upper($1) WHERE "id" = $2`
	_, err := pool.Exec(ctx, sqlBad, "hi", int64(1))
	if err == nil {
		t.Fatalf("expected unsupported error, got nil")
	}
	if !errors.Is(err, simsql.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// TestCompositePKRoundTrip exercises a two-column PK end-to-end.
// Mirrors entities like `hypertable PurchaseEvent` whose codegen emits
// composite-PK SQL.
func TestCompositePKRoundTrip(t *testing.T) {
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "vendor_purchase_event",
		Cols: []sim.Column{
			{Name: "purchase_id", Kind: sim.KindInt64},
			{Name: "occurred_at", Kind: sim.KindInt64}, // surrogate for time
			{Name: "amount", Kind: sim.KindInt64},
		},
		PKCols: []string{"purchase_id", "occurred_at"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	pool := sim.NewPool(cat)
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."vendor_purchase_event" ("purchase_id", "occurred_at", "amount") VALUES ($1, $2, $3) RETURNING "purchase_id"`
	if err := pool.QueryRow(ctx, sqlIns, int64(7), int64(100), int64(500)).Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const sqlGet = `SELECT "amount" FROM "atlantis"."vendor_purchase_event" WHERE "purchase_id" = $1 AND "occurred_at" = $2`
	var amount int64
	if err := pool.QueryRow(ctx, sqlGet, int64(7), int64(100)).Scan(&amount); err != nil {
		t.Fatalf("get: %v", err)
	}
	if amount != 500 {
		t.Fatalf("composite PK get: got %d want 500", amount)
	}

	const sqlUpd = `UPDATE "atlantis"."vendor_purchase_event" SET "amount" = $1 WHERE "purchase_id" = $2 AND "occurred_at" = $3`
	tag, err := pool.Exec(ctx, sqlUpd, int64(999), int64(7), int64(100))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("update RowsAffected: got %d want 1", tag.RowsAffected())
	}

	if err := pool.QueryRow(ctx, sqlGet, int64(7), int64(100)).Scan(&amount); err != nil {
		t.Fatalf("post-update get: %v", err)
	}
	if amount != 999 {
		t.Fatalf("post-update amount: got %d want 999", amount)
	}
}
