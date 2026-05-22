//go:build integration

// Package backfill_test exercises the chunked-UPDATE worker end-to-end
// against a real Postgres via testcontainers. The tests live in a
// sub-package so they compile independently of tests/integration's
// gen/-dependent files (grpc_test.go, crud_test.go), which require
// `make codegen` to have populated a caller's typed SDK first.
package backfill_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/backfill"
)

// seedTestUsers creates a small populated table that the backfill tests
// drive against. display_name starts nullable; the worker populates it
// via the backfill expression, then Phase 3 runs SET NOT NULL.
func seedTestUsers(t *testing.T, pool *pgxpool.Pool, ctx context.Context, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS atlantis.test_users (
    id           BIGINT PRIMARY KEY,
    first_name   TEXT NOT NULL,
    last_name    TEXT NOT NULL,
    display_name TEXT
);
TRUNCATE atlantis.test_users;
`); err != nil {
		t.Fatalf("create test_users: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.test_users (id, first_name, last_name)
SELECT i, 'first' || i::text, 'last' || i::text
FROM generate_series(1, $1) AS i`, n); err != nil {
		t.Fatalf("seed test_users: %v", err)
	}
}

// seedBackfillPlan inserts the plan + field-state rows the worker
// claims. lastPK > 0 simulates resume-from-cursor.
func seedBackfillPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context, planHash string, lastPK int64) {
	t.Helper()
	postSQL := `ALTER TABLE atlantis.test_users ALTER COLUMN display_name SET NOT NULL;`
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.backfill_plan
    (plan_hash, caller, status, post_sql, ir_checkpoint_hash)
VALUES ($1, 'integration-test', 'phase2_running', $2, '')`,
		planHash, postSQL); err != nil {
		t.Fatalf("insert backfill_plan: %v", err)
	}
	var lastPKStr *string
	if lastPK > 0 {
		s := strconv.FormatInt(lastPK, 10)
		lastPKStr = &s
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.backfill_field_state
    (plan_hash, entity_id, field, expression, pk_column, table_name, last_pk, status)
VALUES ($1, 'test.User', 'display_name', $2, 'id', '"atlantis"."test_users"', $3, 'pending')`,
		planHash,
		`first_name || ' ' || last_name`,
		lastPKStr); err != nil {
		t.Fatalf("insert backfill_field_state: %v", err)
	}
}

func startBackfillWorker(t *testing.T, pool *pgxpool.Pool) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := backfill.NewWorker(pool, backfill.Config{
		Schema:       "atlantis",
		PollInterval: 50 * time.Millisecond,
		ChunkSize:    500,
		Throttle:     5 * time.Millisecond,
	})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	return cancel, done
}

func waitForPlanStatus(t *testing.T, pool *pgxpool.Pool, ctx context.Context, planHash, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT status FROM atlantis.backfill_plan WHERE plan_hash=$1`, planHash,
		).Scan(&got); err == nil && got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var got string
	_ = pool.QueryRow(ctx,
		`SELECT status FROM atlantis.backfill_plan WHERE plan_hash=$1`, planHash,
	).Scan(&got)
	t.Fatalf("plan %s did not reach %s within %s (current=%s)", planHash, want, timeout, got)
}

func TestBackfill_SmallEndToEnd(t *testing.T) {
	h := newPGHarness(t)
	pool := h.Pool
	ctx := context.Background()

	seedTestUsers(t, pool, ctx, 1500) // > 3 chunks at ChunkSize=500
	seedBackfillPlan(t, pool, ctx, "plan-small-e2e", 0)

	cancel, done := startBackfillWorker(t, pool)
	defer func() { cancel(); <-done }()

	waitForPlanStatus(t, pool, ctx, "plan-small-e2e", "complete", 30*time.Second)

	var nullCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM atlantis.test_users WHERE display_name IS NULL`,
	).Scan(&nullCount); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nullCount != 0 {
		t.Errorf("expected 0 NULL display_name rows, got %d", nullCount)
	}

	var sample string
	if err := pool.QueryRow(ctx,
		`SELECT display_name FROM atlantis.test_users WHERE id=42`,
	).Scan(&sample); err != nil {
		t.Fatalf("sample row: %v", err)
	}
	if sample != "first42 last42" {
		t.Errorf("expression mis-evaluated: got %q want %q", sample, "first42 last42")
	}

	// Phase 3's SET NOT NULL must have landed.
	var notNull bool
	if err := pool.QueryRow(ctx, `
SELECT is_nullable = 'NO'
FROM information_schema.columns
WHERE table_schema='atlantis' AND table_name='test_users' AND column_name='display_name'`,
	).Scan(&notNull); err != nil {
		t.Fatalf("inspect column: %v", err)
	}
	if !notNull {
		t.Errorf("display_name should be NOT NULL after Phase 3")
	}

	var rowsProcessed int64
	var fieldStatus string
	if err := pool.QueryRow(ctx, `
SELECT status, rows_processed FROM atlantis.backfill_field_state
WHERE plan_hash='plan-small-e2e' AND entity_id='test.User' AND field='display_name'`,
	).Scan(&fieldStatus, &rowsProcessed); err != nil {
		t.Fatalf("inspect field_state: %v", err)
	}
	if fieldStatus != "complete" {
		t.Errorf("field status = %q, want complete", fieldStatus)
	}
	if rowsProcessed != 1500 {
		t.Errorf("rows_processed = %d, want 1500", rowsProcessed)
	}
}

func TestBackfill_ResumeFromMidChunkCursor(t *testing.T) {
	// Simulates a pod restart mid-backfill by seeding the field-state row
	// with last_pk already set partway through. The fresh worker should
	// resume above that cursor — older rows stay untouched (proving no
	// double-process), the rest get backfilled.
	h := newPGHarness(t)
	pool := h.Pool
	ctx := context.Background()

	seedTestUsers(t, pool, ctx, 1000)
	if _, err := pool.Exec(ctx, `
UPDATE atlantis.test_users SET display_name = 'PREEXISTING'
WHERE id <= 400`); err != nil {
		t.Fatalf("preseed: %v", err)
	}
	seedBackfillPlan(t, pool, ctx, "plan-resume", 400)

	cancel, done := startBackfillWorker(t, pool)
	defer func() { cancel(); <-done }()

	waitForPlanStatus(t, pool, ctx, "plan-resume", "complete", 30*time.Second)

	var preexistingKept int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM atlantis.test_users WHERE id <= 400 AND display_name = 'PREEXISTING'`,
	).Scan(&preexistingKept); err != nil {
		t.Fatalf("count preexisting: %v", err)
	}
	if preexistingKept != 400 {
		t.Errorf("preexisting markers overwritten: %d/400 kept", preexistingKept)
	}

	var processed int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM atlantis.test_users WHERE id > 400 AND display_name = ('first' || id::text || ' last' || id::text)`,
	).Scan(&processed); err != nil {
		t.Fatalf("count processed: %v", err)
	}
	if processed != 600 {
		t.Errorf("rows above cursor backfilled: %d/600", processed)
	}
}

func TestBackfill_DuplicatePlanHashRejected(t *testing.T) {
	// backfill_plan.plan_hash is PRIMARY KEY; double-submission fails at
	// the DB layer. The admin RPC catches this earlier with the idempotency
	// check, but the DB constraint is the load-bearing guard — a future
	// RPC refactor mustn't accidentally allow duplicate rows.
	h := newPGHarness(t)
	pool := h.Pool
	ctx := context.Background()

	seedTestUsers(t, pool, ctx, 10)
	seedBackfillPlan(t, pool, ctx, "plan-dup", 0)

	_, err := pool.Exec(ctx, `
INSERT INTO atlantis.backfill_plan
    (plan_hash, caller, status, post_sql, ir_checkpoint_hash)
VALUES ('plan-dup', 'integration-test', 'phase2_running', '', '')`)
	if err == nil {
		t.Fatalf("expected PRIMARY KEY violation on duplicate plan_hash")
	}
	if !strings.Contains(err.Error(), "duplicate key") &&
		!strings.Contains(err.Error(), "unique constraint") {
		t.Errorf("error should mention duplicate/unique key, got: %v", err)
	}
}

func TestBackfill_FailedChunkMarksFieldFailed(t *testing.T) {
	// Bad expression — references a column the table doesn't have. The
	// chunked UPDATE errors at first execution, worker marks the field
	// failed, Phase 3 never runs, column stays nullable.
	h := newPGHarness(t)
	pool := h.Pool
	ctx := context.Background()

	seedTestUsers(t, pool, ctx, 100)
	postSQL := `ALTER TABLE atlantis.test_users ALTER COLUMN display_name SET NOT NULL;`
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.backfill_plan
    (plan_hash, caller, status, post_sql, ir_checkpoint_hash)
VALUES ('plan-bad-expr', 'integration-test', 'phase2_running', $1, '')`, postSQL); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.backfill_field_state
    (plan_hash, entity_id, field, expression, pk_column, table_name, status)
VALUES ('plan-bad-expr', 'test.User', 'display_name',
        'nonexistent_column_xyz', 'id', '"atlantis"."test_users"', 'pending')`); err != nil {
		t.Fatalf("insert field: %v", err)
	}

	cancel, done := startBackfillWorker(t, pool)
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(20 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		_ = pool.QueryRow(ctx,
			`SELECT status FROM atlantis.backfill_field_state WHERE plan_hash='plan-bad-expr'`,
		).Scan(&status)
		if status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != "failed" {
		t.Fatalf("field never reached failed; status=%s", status)
	}

	var planStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM atlantis.backfill_plan WHERE plan_hash='plan-bad-expr'`,
	).Scan(&planStatus); err != nil {
		t.Fatalf("plan lookup: %v", err)
	}
	if planStatus == "complete" {
		t.Errorf("plan should not complete with a failed field; got status=%s", planStatus)
	}

	var notNull bool
	_ = pool.QueryRow(ctx, `
SELECT is_nullable = 'NO'
FROM information_schema.columns
WHERE table_schema='atlantis' AND table_name='test_users' AND column_name='display_name'`,
	).Scan(&notNull)
	if notNull {
		t.Errorf("display_name should still be nullable when a field failed")
	}
}
