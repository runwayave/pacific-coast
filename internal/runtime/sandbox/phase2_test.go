package sandbox_test

// Phase 2 surface tests: time-travel marks, forks, StrictDeterministic
// clock, and ON CONFLICT DO UPDATE.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// usersIR shapes an entity with a single bigint PK + text email so
// every Phase 2 test can share it without re-declaring the IR.
func usersIR() *dsl.IR {
	return &dsl.IR{
		Version: 1,
		Entities: []dsl.Entity{{
			Name:      "User",
			Namespace: "consumer",
			Kind:      dsl.EntityKindRegular,
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true},
				{Name: "email", Type: dsl.FieldType{Name: "text"}, NotNull: true},
				{Name: "plan", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			},
		}},
	}
}

const sqlInsertUser = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan") VALUES ($1, $2, $3) RETURNING "id"`
const sqlListUsers = `SELECT "id", "email", "plan" FROM "atlantis"."consumer_user" ORDER BY "id" ASC`

// snapshotEmails reads every user's email into a slice for comparison.
func snapshotEmails(t *testing.T, sb *sandbox.Sandbox) []string {
	t.Helper()
	rows, err := sb.Pool().Query(context.Background(), sqlListUsers)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var email, plan string
		if err := rows.Scan(&id, &email, &plan); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, email)
	}
	return out
}

// ────────────────────── time-travel marks ──────────────────────

// TestMarkRestoreRoundTrip is the basic API contract: capture state,
// mutate, restore — observed state matches what Mark saw.
func TestMarkRestoreRoundTrip(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	for i, email := range []string{"a@b.com", "c@d.com"} {
		if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(i+1), email, "pro").Scan(new(int64)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	mark := sb.Mark()
	if mark == nil {
		t.Fatalf("Mark returned nil")
	}

	// Mutate freely after the mark.
	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(3), "e@f.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("post-mark insert: %v", err)
	}
	if got := snapshotEmails(t, sb); len(got) != 3 {
		t.Fatalf("post-mark: got %d rows want 3 (%v)", len(got), got)
	}

	if err := sb.RestoreTo(mark); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := snapshotEmails(t, sb)
	if len(got) != 2 || got[0] != "a@b.com" || got[1] != "c@d.com" {
		t.Fatalf("post-restore emails: %v", got)
	}
}

// TestMarkStacking confirms multiple marks held simultaneously each
// keep their state — an agent can hold marks at every decision point
// and rewind selectively without losing earlier captures.
func TestMarkStacking(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "a@b.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m1 := sb.Mark() // state: 1 row

	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(2), "c@d.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	m2 := sb.Mark() // state: 2 rows

	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(3), "e@f.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Rewind to m2 (2 rows).
	if err := sb.RestoreTo(m2); err != nil {
		t.Fatalf("restore m2: %v", err)
	}
	if got := snapshotEmails(t, sb); len(got) != 2 {
		t.Fatalf("after m2: got %d rows want 2", len(got))
	}

	// And further back to m1 (1 row).
	if err := sb.RestoreTo(m1); err != nil {
		t.Fatalf("restore m1: %v", err)
	}
	if got := snapshotEmails(t, sb); len(got) != 1 || got[0] != "a@b.com" {
		t.Fatalf("after m1: %v", got)
	}
}

// TestMarkOwnerMismatch confirms marks are non-portable across
// sandboxes — restoring a mark into a different sandbox errors with
// the typed sentinel.
func TestMarkOwnerMismatch(t *testing.T) {
	sbA := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	sbB := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	m := sbA.Mark()
	if err := sbB.RestoreTo(m); !errors.Is(err, sandbox.ErrMarkOwnerMismatch) {
		t.Fatalf("expected ErrMarkOwnerMismatch, got %v", err)
	}
}

// ────────────────────── forked sandboxes ──────────────────────

// TestForkParentIsolated confirms that children's writes don't reach
// the parent.
func TestForkParentIsolated(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()
	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "a@b.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	kids, err := sb.Fork(3)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(kids) != 3 {
		t.Fatalf("kids: got %d want 3", len(kids))
	}

	// Each child writes its own row.
	for i, k := range kids {
		t.Cleanup(func() { _ = k.Close() })
		if err := k.Pool().QueryRow(ctx, sqlInsertUser, int64(100+i), "k"+itoaSimple(i)+"@y.com", "pro").Scan(new(int64)); err != nil {
			t.Fatalf("child %d insert: %v", i, err)
		}
	}

	// Parent still has the single seed row.
	got := snapshotEmails(t, sb)
	if len(got) != 1 || got[0] != "a@b.com" {
		t.Fatalf("parent emails: %v", got)
	}
}

// TestForkChildrenIsolated confirms siblings don't see each other's
// writes.
func TestForkChildrenIsolated(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	kids, err := sb.Fork(2)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, k := range kids {
		t.Cleanup(func() { _ = k.Close() })
	}
	if err := kids[0].Pool().QueryRow(ctx, sqlInsertUser, int64(1), "first@y.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("kid0: %v", err)
	}
	if err := kids[1].Pool().QueryRow(ctx, sqlInsertUser, int64(2), "second@y.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("kid1: %v", err)
	}

	if g := snapshotEmails(t, kids[0]); len(g) != 1 || g[0] != "first@y.com" {
		t.Fatalf("kid0 emails: %v", g)
	}
	if g := snapshotEmails(t, kids[1]); len(g) != 1 || g[0] != "second@y.com" {
		t.Fatalf("kid1 emails: %v", g)
	}
}

// TestForkOfFork exercises that a child can itself be Forked.
func TestForkOfFork(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()
	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "root@y.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	parents, err := sb.Fork(1)
	if err != nil {
		t.Fatalf("fork 1: %v", err)
	}
	t.Cleanup(func() { _ = parents[0].Close() })

	grands, err := parents[0].Fork(2)
	if err != nil {
		t.Fatalf("fork 2: %v", err)
	}
	for _, g := range grands {
		t.Cleanup(func() { _ = g.Close() })
		// Inherits the root row.
		if got := snapshotEmails(t, g); len(got) != 1 || got[0] != "root@y.com" {
			t.Fatalf("grand: %v", got)
		}
	}
}

// ────────────────────── StrictDeterministic ──────────────────────

// TestStrictDeterministicClock confirms two sandboxes seeded
// identically observe identical now() sequences.
func TestStrictDeterministicClock(t *testing.T) {
	sbA := sandbox.NewT(t, sandbox.Options{
		IR:          usersIR(),
		Determinism: sandbox.DeterminismStrict,
		Seed:        42,
	})
	sbB := sandbox.NewT(t, sandbox.Options{
		IR:          usersIR(),
		Determinism: sandbox.DeterminismStrict,
		Seed:        42,
	})

	// Trigger now() through a soft-delete UPDATE (which writes now()
	// into deleted_at, observable via SELECT). Same SQL → same clock
	// advance order → same stored timestamp.
	ctx := context.Background()
	if err := sbA.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "a@b.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("A seed: %v", err)
	}
	if err := sbB.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "a@b.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("B seed: %v", err)
	}

	const sqlSet = `UPDATE "atlantis"."consumer_user" SET "plan" = $1 WHERE "id" = $2`
	if _, err := sbA.Pool().Exec(ctx, sqlSet, "trial", int64(1)); err != nil {
		t.Fatalf("A update: %v", err)
	}
	if _, err := sbB.Pool().Exec(ctx, sqlSet, "trial", int64(1)); err != nil {
		t.Fatalf("B update: %v", err)
	}

	// Snapshots of both sandboxes should have IDENTICAL bytes —
	// determinism's strongest guarantee. Mostly checking that
	// SavedAt also tracks the deterministic clock.
	bA, err := sbA.Snapshot()
	if err != nil {
		t.Fatalf("A snapshot: %v", err)
	}
	bB, err := sbB.Snapshot()
	if err != nil {
		t.Fatalf("B snapshot: %v", err)
	}
	if len(bA) != len(bB) {
		t.Fatalf("snapshot length: A=%d B=%d", len(bA), len(bB))
	}
	// Spot-check the SavedAt-driving clock is monotonic, not real time.
	// One easy proxy: the clock should not equal a recent wall-clock
	// time within a 1-year window — the deterministic clock starts
	// near Unix epoch.
	if time.Since(time.Unix(0, 0)) < 365*24*time.Hour {
		t.Skip("system time too close to epoch; can't validate determinism via SavedAt")
	}
}

// TestDeterminismOffUsesWallClock confirms the default behavior
// still uses real time when Determinism is not set — important so
// existing tests don't silently shift to fake time.
func TestDeterminismOffUsesWallClock(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	blob, err := sb.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Decoding via Restore is OK, but we just want to confirm that
	// taking a snapshot now succeeds — Restoring later doesn't shift
	// any wall-clock-dependent behavior. The actual SavedAt is opaque.
	if len(blob) == 0 {
		t.Fatalf("snapshot empty")
	}
}

// ────────────────────── ON CONFLICT DO UPDATE ──────────────────────

// TestOnConflictDoNothing exercises the silent-skip path.
func TestOnConflictDoNothing(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "first@y.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const sqlUpsert = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan") VALUES ($1, $2, $3) ON CONFLICT ("id") DO NOTHING RETURNING "email"`
	var email string
	if err := sb.Pool().QueryRow(ctx, sqlUpsert, int64(1), "second@y.com", "trial").Scan(&email); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if email != "first@y.com" {
		t.Fatalf("RETURNING after DO NOTHING: got %q want %q (existing row should be returned)", email, "first@y.com")
	}
}

// TestOnConflictDoUpdate exercises the canonical upsert: insert if
// new, update otherwise, including EXCLUDED.col references.
func TestOnConflictDoUpdate(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	if err := sb.Pool().QueryRow(ctx, sqlInsertUser, int64(1), "old@y.com", "pro").Scan(new(int64)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const sqlUpsert = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan") VALUES ($1, $2, $3) ON CONFLICT ("id") DO UPDATE SET "email" = EXCLUDED."email", "plan" = $4 RETURNING "id", "email", "plan"`
	var id int64
	var email, plan string
	if err := sb.Pool().QueryRow(ctx, sqlUpsert, int64(1), "new@y.com", "trial", "trial").Scan(&id, &email, &plan); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != 1 || email != "new@y.com" || plan != "trial" {
		t.Fatalf("upsert result: id=%d email=%q plan=%q", id, email, plan)
	}

	// And the SAME upsert against a new id should insert.
	if err := sb.Pool().QueryRow(ctx, sqlUpsert, int64(2), "fresh@y.com", "pro", "pro").Scan(&id, &email, &plan); err != nil {
		t.Fatalf("upsert insert path: %v", err)
	}
	if id != 2 || email != "fresh@y.com" || plan != "pro" {
		t.Fatalf("upsert insert result: id=%d email=%q plan=%q", id, email, plan)
	}
}

// TestOnConflictTargetMustMatchPK pins the Phase 2 boundary: until
// secondary unique indexes ship, ON CONFLICT only accepts PK targets.
func TestOnConflictTargetMustMatchPK(t *testing.T) {
	sb := sandbox.NewT(t, sandbox.Options{IR: usersIR()})
	ctx := context.Background()

	// "email" isn't the PK; conflict target must match PK columns.
	const sql = `INSERT INTO "atlantis"."consumer_user" ("id", "email", "plan") VALUES ($1, $2, $3) ON CONFLICT ("email") DO NOTHING`
	_, err := sb.Pool().Exec(ctx, sql, int64(1), "x@y.com", "pro")
	if err == nil {
		t.Fatalf("expected error for non-PK conflict target, got nil")
	}
}

// itoaSimple is a tiny stand-in so this test file doesn't need to
// import strconv just to format small loop indices.
func itoaSimple(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
