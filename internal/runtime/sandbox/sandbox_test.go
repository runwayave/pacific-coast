package sandbox_test

// Sandbox-level tests: end-to-end through atl.NewT, including the Cache
// and Outbox stubs that the generated server consumes.

import (
	"context"
	"errors"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// makeUserSandbox builds a typical "consumer.user" entity through the
// public façade, so we exercise NewT + the wired Cache/Outbox at the
// same time.
func makeUserSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	sb := sandbox.NewT(t, sandbox.Options{})
	if err := sb.Catalog().RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "consumer_user",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "email", Kind: sim.KindString},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return sb
}

// TestNewTSandboxRoundTrip confirms the NewT helper produces a Sandbox
// whose Pool round-trips a basic INSERT + GET — the smallest possible
// "did the façade wire up correctly" assertion.
func TestNewTSandboxRoundTrip(t *testing.T) {
	sb := makeUserSandbox(t)
	ctx := context.Background()

	const sqlIns = `INSERT INTO "atlantis"."consumer_user" ("id", "email") VALUES ($1, $2) RETURNING "id"`
	var id int64
	if err := sb.Pool().QueryRow(ctx, sqlIns, int64(1), "a@b.com").Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != 1 {
		t.Fatalf("RETURNING: got %d want 1", id)
	}
}

// TestCacheAlwaysMisses pins the runtime.Cache stub contract: Get
// always returns ErrCacheMiss; the generated handler falls through to
// the DB on every read. Set is recorded for inspection.
func TestCacheAlwaysMisses(t *testing.T) {
	sb := makeUserSandbox(t)
	ctx := context.Background()

	if _, err := sb.Cache().Get(ctx, "atl:v1:user:1:7"); !errors.Is(err, runtime.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}

	if err := sb.Cache().Set(ctx, "atl:v1:user:1:7", []byte("body"), 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	sets := sb.CacheStub().LastSets()
	if len(sets) != 1 {
		t.Fatalf("recorded sets: got %d want 1", len(sets))
	}
	if sets[0].Key != "atl:v1:user:1:7" {
		t.Fatalf("set[0].Key: got %q want %q", sets[0].Key, "atl:v1:user:1:7")
	}

	// CurrentVersion always returns 0 (treated as "no pointer; miss")
	// by generated code.
	v, err := sb.Cache().CurrentVersion(ctx, "user", "1")
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if v != 0 {
		t.Fatalf("CurrentVersion: got %d want 0", v)
	}
}

// TestOutboxRecordsInvalidations pins the runtime.Outbox stub: both
// Enqueue and EnqueueGenerationBump are recorded for tests to inspect.
func TestOutboxRecordsInvalidations(t *testing.T) {
	sb := makeUserSandbox(t)
	ctx := context.Background()

	tx, err := sb.Pool().BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := sb.Outbox().Enqueue(ctx, tx, "user", "1", 42); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := sb.Outbox().EnqueueGenerationBump(ctx, tx, "user"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	enq, bumps := sb.OutboxStub().Recorded()
	if len(enq) != 1 {
		t.Fatalf("enqueues: got %d want 1", len(enq))
	}
	if enq[0].Entity != "user" || enq[0].ID != "1" || enq[0].NewVersion != 42 {
		t.Fatalf("enqueue[0]: %+v", enq[0])
	}
	if len(bumps) != 1 || bumps[0].Entity != "user" {
		t.Fatalf("bumps: %+v", bumps)
	}
}

// TestBackendErrorOnEmbeddedPhase0 confirms the loud-error stance for
// backends the simulator doesn't support yet. The embedded backend
// session) implements embedded; until then, asking for it explicitly
// is a clear error rather than a silent sim fallback.
func TestBackendErrorOnEmbeddedPhase0(t *testing.T) {
	_, err := sandbox.New(sandbox.Options{Backend: sandbox.BackendEmbedded})
	if err == nil {
		t.Fatalf("expected backend error for embedded, got nil")
	}
}
