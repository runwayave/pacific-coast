// Package-level SQL helpers shared between the SDK Worker (direct-PG
// drain loop) and the server-side dispatcher (Temporal-style worker
// poll over gRPC).
//
// The same primitives — claim a batch, extend lease, mark complete,
// report failure, move to DLQ, release row — drive both code paths.
// Centralising them here means the dispatcher and the SDK Worker
// agree on the exact SQL semantics (predicate shape, lease format,
// terminal-state writes) without one drifting from the other. The
// claim CTE in particular MUST stay byte-identical: SKIP LOCKED is
// what makes a direct-PG worker and a dispatcher session safe to
// coexist on the same queue, so any difference would break the
// safety story.
//
// Schema is hardcoded to `atlantis.jobs` — the existing Worker
// already does this, and the dispatcher inherits the same convention.
// If atlantis ever runs against a non-default schema, this is the
// one file to update.

package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkerKindDirectPG is the value stamped into atlantis.jobs.worker_kind
// when an SDK direct-PG Worker claims a row. The other valid value,
// `dispatched`, is stamped by the server-side dispatcher in
// internal/server/jobsdispatcher.
const WorkerKindDirectPG = "direct_pg"

// WorkerKindDispatched marks rows claimed by the server-side dispatcher
// on behalf of a remote worker stream.
const WorkerKindDispatched = "dispatched"

// ClaimedRow carries everything the handler-dispatch path needs without
// re-querying the row. Exported so the server-side dispatcher can route
// rows to worker sessions; the field set is the same one the direct-PG
// Worker has always consumed.
type ClaimedRow struct {
	ID           int64
	JobName      string
	Args         []byte
	Attempts     int
	MaxRetries   int
	TimeoutMS    int
	EnqueuedAt   time.Time
	ScheduledFor time.Time
	TraceCtx     []byte
}

// buildClaimSQL renders the claim CTE. Exported as a package-level
// helper so tests can verify the SQL shape (presence of worker_kind /
// worker_session_id stamps, predicate matches partial index) without
// a live PG connection. The string interpolation is bounded to the
// jobNamesFilter clause, which is empty or a static `AND job_name =
// ANY($5::text[])` — no user input flows through this concatenation.
func buildClaimSQL(jobNamesFilter bool) string {
	const head = `
WITH ready AS (
    SELECT id
    FROM atlantis.jobs
    WHERE queue = $1
      AND scheduled_for <= now()
      AND (
        status = 'pending'
        OR (status = 'running' AND (claimed_until IS NULL OR claimed_until < now()))
      )`
	const tail = `
    ORDER BY scheduled_for
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE atlantis.jobs j
   SET status            = 'running',
       attempts          = j.attempts + 1,
       claimed_by        = $3,
       claimed_until     = $4,
       started_at        = COALESCE(j.started_at, now()),
       worker_kind       = $5,
       worker_session_id = NULLIF($6, '')
  FROM ready
 WHERE j.id = ready.id
RETURNING j.id, j.job_name, j.args, j.attempts, j.max_retries,
          COALESCE(j.timeout_ms, 0), j.enqueued_at, j.scheduled_for, j.trace_ctx`
	if jobNamesFilter {
		// $7 binds the job-name allowlist when the caller restricts which
		// declarations they'll handle (the dispatcher uses this to filter
		// to the union of JobNames across connected worker sessions).
		return head + "\n      AND job_name = ANY($7::text[])" + tail
	}
	return head + tail
}

// ClaimRows atomically transitions up to `limit` rows from pending (or
// lease-expired running) to running, stamping the supplied workerKind
// and sessionID for forensic provenance.
//
// jobNames, when non-empty, restricts the claim to rows whose job_name
// is in the allowlist — used by the dispatcher to claim only what its
// connected sessions can handle. When empty, every job_name is eligible
// (the direct-PG Worker calls it this way for backward compatibility).
//
// claimedBy is the lease holder identifier — pod-id for direct-PG,
// "dispatcher/<sessionID>" for the dispatcher. Used by ExtendLease's
// predicate to ensure only the current claimant can heartbeat.
//
// leaseUntil is the absolute deadline written to claimed_until. The
// caller computes this as now() + HeartbeatBudget; we don't compute
// here so the same wall-clock is used for both the row update and any
// in-process bookkeeping.
//
// workerKind is one of the WorkerKind* constants. sessionID is the
// dispatcher session id (empty for direct-PG).
func ClaimRows(
	ctx context.Context,
	pool *pgxpool.Pool,
	queue string,
	jobNames []string,
	limit int,
	claimedBy string,
	leaseUntil time.Time,
	workerKind string,
	sessionID string,
) ([]ClaimedRow, error) {
	args := []any{queue, limit, claimedBy, leaseUntil, workerKind, sessionID}
	sqlText := buildClaimSQL(false)
	if len(jobNames) > 0 {
		sqlText = buildClaimSQL(true)
		args = append(args, jobNames)
	}
	rs, err := pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []ClaimedRow
	for rs.Next() {
		var r ClaimedRow
		if err := rs.Scan(&r.ID, &r.JobName, &r.Args, &r.Attempts, &r.MaxRetries,
			&r.TimeoutMS, &r.EnqueuedAt, &r.ScheduledFor, &r.TraceCtx); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// ExtendLease bumps claimed_until on every row in jobIDs that still
// belongs to claimedBy. The claimed_by predicate guards against bumping
// a lease that's been reassigned to a peer after a session crash.
//
// Batched by design: the dispatcher uses a single Heartbeat envelope
// per session to lease-extend every in-flight job in one round-trip;
// the direct-PG Worker calls this with a one-element slice. Both
// behave identically — the SQL is the same, the parameter binding
// differs only in slice length.
//
// Idempotent: a duplicate ExtendLease for the same ids is a no-op
// write that only shifts claimed_until forward.
func ExtendLease(
	ctx context.Context,
	pool *pgxpool.Pool,
	jobIDs []int64,
	claimedBy string,
	leaseExtension time.Duration,
) error {
	if len(jobIDs) == 0 {
		return nil
	}
	// Numeric multiplication (not string-concat) so PG infers $1 as
	// bigint and pgx encodes the int64 milliseconds directly. The
	// string-concat form (`$1 || ' milliseconds'`) made PG infer $1
	// as text, which made pgx error with "cannot find encode plan"
	// at production.
	_, err := pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET claimed_until = now() + ($1 * interval '1 millisecond')
 WHERE id = ANY($2::bigint[]) AND claimed_by = $3`,
		leaseExtension.Milliseconds(), jobIDs, claimedBy)
	return err
}

// MarkComplete writes the terminal state for a successful job. Wrapped
// in its own statement — the handler's tx (if any) already committed,
// so this is a discrete "I'm done" write.
func MarkComplete(ctx context.Context, pool *pgxpool.Pool, jobID int64) error {
	_, err := pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET status            = 'complete',
       completed_at      = now(),
       claimed_by        = NULL,
       claimed_until     = NULL,
       worker_session_id = NULL
 WHERE id = $1`, jobID)
	return err
}

// ReportFailure records a handler error. If attempts has exhausted
// max_retries, the row moves to atlantis.jobs_dead via MoveToDLQ;
// otherwise it resets to pending so the next drain pass retries.
//
// attempts is the post-claim value (the claim CTE already incremented
// it); the caller passes the value from ClaimedRow.Attempts.
func ReportFailure(
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID int64,
	attempts int,
	maxRetries int,
	errMsg string,
) error {
	if attempts >= maxRetries {
		return MoveToDLQ(ctx, pool, jobID, errMsg)
	}
	_, err := pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET status            = 'pending',
       last_error        = $1,
       last_error_at     = now(),
       claimed_by        = NULL,
       claimed_until     = NULL,
       worker_session_id = NULL
 WHERE id = $2`, errMsg, jobID)
	return err
}

// MoveToDLQ atomically removes a row from atlantis.jobs and inserts the
// matching row into atlantis.jobs_dead. The INSERT preserves the row id
// so a subsequent RetryDeadJob can route by id.
//
// All-in-one tx: a torn move (deleted from jobs but not inserted into
// jobs_dead, or vice versa) would leak rows. The tx wraps the INSERT +
// DELETE atomically.
func MoveToDLQ(ctx context.Context, pool *pgxpool.Pool, jobID int64, errMsg string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.jobs_dead
    (id, job_name, queue, args, attempts, max_retries, last_error, last_error_at, enqueued_at, submitted_by)
SELECT id, job_name, queue, args, attempts, max_retries, $2, now(), enqueued_at, submitted_by
FROM atlantis.jobs WHERE id = $1`, jobID, errMsg); err != nil {
		return fmt.Errorf("insert dead: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM atlantis.jobs WHERE id = $1`, jobID); err != nil {
		return fmt.Errorf("delete jobs: %w", err)
	}
	return tx.Commit(ctx)
}

// ReleaseRow returns a row to pending state without touching attempts
// or last_error. Used by the dispatcher when a session closes mid-
// flight or when a claimed row finds no eligible session to dispatch
// to (race: the only session for this job_name disconnected between
// claim and route).
//
// claimedBy is a guard: the release only takes effect if the row still
// belongs to the supplied lease holder. Prevents a stale unregister
// from clobbering a fresh claim by a sibling worker.
//
// reason is recorded as last_error so an operator can grep for
// "released:session_close" etc. when triaging stuck queues. It's
// informational only; the row goes back to pending and is eligible
// for the next drain pass.
func ReleaseRow(
	ctx context.Context,
	pool *pgxpool.Pool,
	jobID int64,
	claimedBy string,
	reason string,
) error {
	_, err := pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET status            = 'pending',
       last_error        = $3,
       last_error_at     = now(),
       claimed_by        = NULL,
       claimed_until     = NULL,
       worker_session_id = NULL
 WHERE id = $1 AND claimed_by = $2`, jobID, claimedBy, "released:"+reason)
	return err
}

// PgListen subscribes to a Postgres LISTEN channel and pushes a wake
// signal onto notifyCh for every notification whose payload satisfies
// the filter predicate. Runs until ctx is canceled, transparently
// reconnecting after any session-ending error.
//
// The function is goroutine-shaped: the caller launches it via `go`
// and owns notifyCh's lifecycle. Recoverable panics inside the inner
// session loop are caught and logged so a malformed notification can't
// take down the calling goroutine.
//
// Reconnect cadence: a 1s gap between failed sessions absorbs flapping
// connections without spinning. The seed-drain that callers typically
// do before launching PgListen covers the gap between process start
// and the first LISTEN session establishing.
func PgListen(
	ctx context.Context,
	pool *pgxpool.Pool,
	channel string,
	filter func(payload string) bool,
	notifyCh chan struct{},
	logger *slog.Logger,
) {
	if logger == nil {
		logger = slog.Default()
	}
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("pg listen loop panic", "channel", channel, "panic", rec)
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := pgListenSession(ctx, pool, channel, filter, notifyCh); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("pg listen session ended", "channel", channel, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func pgListenSession(
	ctx context.Context,
	pool *pgxpool.Pool,
	channel string,
	filter func(payload string) bool,
	notifyCh chan struct{},
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// LISTEN identifier must come from a trusted source — channel is a
	// package constant in every call site today. Validate defensively
	// to fail fast if a future caller passes a user-supplied string.
	if !isSafeIdent(channel) {
		return fmt.Errorf("unsafe LISTEN channel %q", channel)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if filter != nil && !filter(notif.Payload) {
			continue
		}
		select {
		case notifyCh <- struct{}{}:
		default:
			// Channel buffered to 1; a pending signal is already going
			// to wake the consumer, so we coalesce.
		}
	}
}

// isSafeIdent guards the LISTEN identifier interpolation in
// pgListenSession against injection. PG identifiers are letters,
// digits, and underscores; the channel names atlantis uses today
// (`atl_jobs`, possible future `atl_schema`) all satisfy this.
func isSafeIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_':
		default:
			return false
		}
	}
	return true
}
