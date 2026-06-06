package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config tunes the worker pool.
//
// Defaults are picked to be safe on a multi-pod deployment without
// further tuning: 1s drain interval matches LISTEN/NOTIFY's notify-
// or-poll cadence, 50-row batches are small enough to make progress
// visible from /metrics yet large enough that a quiet queue costs ~1
// SQL round-trip per second. Lease defaults assume the typical job
// finishes within timeout * 1.5; tune Lease down for short jobs to
// recover from pod crashes faster.
type Config struct {
	Schema          string
	PodID           string
	BatchSize       int
	DrainInterval   time.Duration
	HeartbeatBudget time.Duration
	Logger          *slog.Logger
}

// DefaultConfig returns a Config with safe defaults populated.
//
// PodID is hostname-pid by default — enough granularity for an
// operator scanning atlantis.jobs to spot which pod owns a stuck
// claim. Override in tests via Config.PodID.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "atlantis"
	}
	return Config{
		Schema:          "atlantis",
		PodID:           fmt.Sprintf("%s-%d", host, os.Getpid()),
		BatchSize:       50,
		DrainInterval:   time.Second,
		HeartbeatBudget: 2 * time.Minute,
		Logger:          slog.Default(),
	}
}

// JobCompleteHook is called by the worker after a job completes or
// fails terminally. Server-internal code (workflow engine) supplies
// an implementation; caller-side workers leave it nil.
type JobCompleteHook interface {
	OnJobComplete(ctx context.Context, jobID int64)
	OnJobFailed(ctx context.Context, jobID int64, errMsg string)
}

// TraceHook lets the server inject OTel distributed-tracing into the
// worker's dispatch path without pulling OTel into the client SDK.
// When nil, tracing is a no-op — the handler runs under the bare ctx.
type TraceHook interface {
	// ResumeTrace reconstructs a parent span from the serialized
	// trace_ctx column and returns a ctx carrying that parent.
	ResumeTrace(ctx context.Context, traceCtxJSON []byte) context.Context
	// StartSpan creates a child span for the handler dispatch and
	// returns the wrapped ctx plus a finish function.
	StartSpan(ctx context.Context, jobName string) (context.Context, func())
}

// Worker drains atlantis.jobs for a specific queue. Multiple workers
// on the same queue across pods coexist safely: claim uses FOR
// UPDATE SKIP LOCKED, and the per-row lease (claimed_until) lets a
// peer recover work from a crashed pod after the lease expires.
//
// A Worker is bound to one queue name. Run two Workers (in different
// goroutines, same Registry) to drain two queues concurrently — the
// SQL is partitioned by queue so they don't contend.
type Worker struct {
	pool     *pgxpool.Pool
	registry *Registry
	queue    string
	cfg      Config

	completeHook JobCompleteHook
	traceHook    TraceHook

	lastClaimNS atomic.Int64
}

// NewWorker constructs a Worker. The registry must be populated
// before Run is called — if a job arrives whose handler isn't
// registered, the worker reports a transient claim error and the
// row stays pending for the next deploy that has the handler.
func NewWorker(pool *pgxpool.Pool, registry *Registry, queue string, cfg Config) *Worker {
	if cfg.Schema == "" {
		cfg.Schema = "atlantis"
	}
	if cfg.PodID == "" {
		cfg.PodID = DefaultConfig().PodID
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = time.Second
	}
	if cfg.HeartbeatBudget <= 0 {
		cfg.HeartbeatBudget = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Worker{
		pool:     pool,
		registry: registry,
		queue:    queue,
		cfg:      cfg,
	}
	w.lastClaimNS.Store(time.Now().UnixNano())
	return w
}

// SetCompleteHook attaches an optional hook called on job completion
// or terminal failure. The server uses this to wire the workflow
// engine; caller-side workers typically leave it nil.
func (w *Worker) SetCompleteHook(h JobCompleteHook) { w.completeHook = h }

// SetTraceHook attaches an optional tracing hook so the worker
// resumes distributed traces from the submitter and creates child
// spans for handler dispatch. Without a hook, tracing is a no-op.
func (w *Worker) SetTraceHook(h TraceHook) { w.traceHook = h }

// Run blocks until ctx is canceled, draining the queue. Errors
// from individual drain passes are logged; only ctx cancellation
// propagates up. The expected production pattern is to launch
// Run in a goroutine from cmd/server/main.go.
func (w *Worker) Run(ctx context.Context) error {
	// Seed-drain pass clears anything that landed before the LISTEN
	// session establishes. The ticker covers steady-state polling
	// for cases where LISTEN's notification got dropped (network
	// blip, pod restart) or where the trigger fired before this pod
	// was subscribing.
	w.drainOnce(ctx)

	ticker := time.NewTicker(w.cfg.DrainInterval)
	defer ticker.Stop()

	notifyCh := make(chan struct{}, 1)
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	go w.runListenLoop(listenCtx, notifyCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drainOnce(ctx)
		case <-notifyCh:
			w.drainOnce(ctx)
		}
	}
}

// LastClaimAt is the wall-clock time of the most recent successful
// claim. Read by /readyz so an operator can spot a worker that's
// fallen asleep (stuck LISTEN connection, deadlocked drain loop)
// well before the queue itself reports lag.
func (w *Worker) LastClaimAt() time.Time {
	return time.Unix(0, w.lastClaimNS.Load())
}

// runListenLoop opens LISTEN sessions on freshly-acquired pool
// connections and reconnects on any session-ending error. Returns
// only when ctx is canceled. The 1s gap between sessions absorbs
// flapping connections without spinning.
func (w *Worker) runListenLoop(ctx context.Context, notifyCh chan struct{}) {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("jobs listen loop panic", "queue", w.queue, "panic", rec)
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := w.runListenSession(ctx, notifyCh); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.cfg.Logger.Warn("jobs listen session ended", "queue", w.queue, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// runListenSession acquires a connection, subscribes to atl_jobs,
// and pushes wake signals onto notifyCh for any notification whose
// payload matches this worker's queue. The session ends on any
// error or ctx cancellation; the caller reconnects.
func (w *Worker) runListenSession(ctx context.Context, notifyCh chan struct{}) error {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN atl_jobs"); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		// Only wake on notifications targeted at our queue. Cross-
		// queue traffic on a shared channel is much cheaper than
		// per-queue channel proliferation in pg's notify subsystem.
		if notif.Payload != w.queue {
			continue
		}
		select {
		case notifyCh <- struct{}{}:
		default:
			// Channel buffered to 1; an existing pending signal is
			// already going to wake the drainer, so we're done.
		}
	}
}

// drainOnce claims a batch and processes each row. Errors from
// individual rows don't abort the batch; the worker logs and moves
// on. A claim batch is a single SQL round-trip; per-row processing
// runs sequentially within the goroutine because handlers can be
// expensive and a stuck handler shouldn't starve sibling handlers.
//
// For concurrent per-job processing, run multiple Workers on the
// same queue — they coordinate via SKIP LOCKED.
func (w *Worker) drainOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("jobs drainOnce panic", "queue", w.queue, "panic", rec)
		}
	}()
	rows, err := w.claim(ctx)
	if err != nil {
		w.cfg.Logger.Warn("jobs claim", "queue", w.queue, "err", err)
		return
	}
	if len(rows) > 0 {
		w.lastClaimNS.Store(time.Now().UnixNano())
	}
	for _, r := range rows {
		w.handleOne(ctx, r)
	}
}

// claimedRow carries everything the handler-dispatch path needs
// without re-querying the row. Kept private — the wire shape (admin
// RPCs) is JobStatus, not this struct.
type claimedRow struct {
	id           int64
	jobName      string
	args         []byte
	attempts     int
	maxRetries   int
	timeoutMS    int
	enqueuedAt   time.Time
	scheduledFor time.Time
	traceCtx     []byte
}

// claim atomically transitions up to BatchSize rows from pending to
// running. The CTE pattern avoids racing pods grabbing the same row
// by combining the SELECT...FOR UPDATE SKIP LOCKED scan with the
// UPDATE in a single statement; CTEs are atomic in Postgres.
//
// The claim filter:
//
//   - queue matches this worker
//   - status is pending OR (status is running AND lease expired) —
//     a crashed pod's row is recoverable
//   - scheduled_for has passed
//
// The lease is set to claimed_until = now() + HeartbeatBudget. We
// rely on the budget being conservative; long-running jobs
// can use the checkpoint API to extend the lease themselves.
func (w *Worker) claim(ctx context.Context) ([]claimedRow, error) {
	leaseDeadline := time.Now().Add(w.cfg.HeartbeatBudget)
	const sqlClaim = `
WITH ready AS (
    SELECT id
    FROM atlantis.jobs
    WHERE queue = $1
      AND scheduled_for <= now()
      AND (
        status = 'pending'
        OR (status = 'running' AND (claimed_until IS NULL OR claimed_until < now()))
      )
    ORDER BY scheduled_for
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE atlantis.jobs j
   SET status        = 'running',
       attempts      = j.attempts + 1,
       claimed_by    = $3,
       claimed_until = $4,
       started_at    = COALESCE(j.started_at, now())
  FROM ready
 WHERE j.id = ready.id
RETURNING j.id, j.job_name, j.args, j.attempts, j.max_retries,
          COALESCE(j.timeout_ms, 0), j.enqueued_at, j.scheduled_for, j.trace_ctx`
	rs, err := w.pool.Query(ctx, sqlClaim, w.queue, w.cfg.BatchSize, w.cfg.PodID, leaseDeadline)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []claimedRow
	for rs.Next() {
		var r claimedRow
		if err := rs.Scan(&r.id, &r.jobName, &r.args, &r.attempts, &r.maxRetries,
			&r.timeoutMS, &r.enqueuedAt, &r.scheduledFor, &r.traceCtx); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// handleOne dispatches a single claimed row through the registry.
// Wrapped in its own deadline (timeout_ms) so a hung handler can't
// freeze the drainer; the lease covers the lease-expiry side of the
// same pact.
//
// Lifecycle:
//
//   - Look up handler in registry. Missing -> bump attempts via
//     reportFailure(transient), leave status='running' until lease
//     expires so a peer with the handler can pick it up.
//   - Handler returns nil -> mark complete in its own tx.
//   - Handler returns err -> bump attempts; if exceeds max_retries,
//     move to atlantis.jobs_dead. Otherwise mark pending again so
//     the next drain pass retries (with last_error_at gating the
//     backoff in claim's predicate, added in a later iteration).
//
// Heartbeat: handlers that need more than HeartbeatBudget should
// extend the lease via the checkpoint API. Callers without the
// checkpoint wiring should keep their timeouts within
// HeartbeatBudget.
func (w *Worker) handleOne(ctx context.Context, r claimedRow) {
	handler := w.registry.Lookup(r.jobName)
	if handler == nil {
		err := &HandlerNotRegisteredError{JobID: r.jobName}
		w.cfg.Logger.Warn("jobs handler missing", "queue", w.queue, "job_id", r.jobName, "row", r.id)
		w.reportTransientFailure(ctx, r, err)
		return
	}

	// Resume the distributed trace from the submitter (if present)
	// and start a worker-side span so the handler's work appears as
	// a child of the submit call. When no TraceHook is installed
	// (common for caller-side workers), tracing is a no-op.
	runCtx := ctx
	endSpan := func() {}
	if w.traceHook != nil {
		runCtx = w.traceHook.ResumeTrace(runCtx, r.traceCtx)
		runCtx, endSpan = w.traceHook.StartSpan(runCtx, r.jobName)
	}
	defer endSpan()

	if r.timeoutMS > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, time.Duration(r.timeoutMS)*time.Millisecond)
		defer cancel()
	}

	runCtx = withCheckpointer(runCtx, newCheckpointer(w.pool, r.id))

	// Lease-extension heartbeat. A goroutine ticks every
	// HeartbeatBudget/3 to bump claimed_until so a peer doesn't
	// poach the row mid-work. The done channel terminates the
	// heartbeat when handler returns.
	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		ticker := time.NewTicker(w.cfg.HeartbeatBudget / 3)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := w.heartbeat(ctx, r.id); err != nil {
					w.cfg.Logger.Warn("jobs heartbeat", "row", r.id, "err", err)
				}
			}
		}
	}()

	err := handler.Handle(runCtx, r.args)
	stopHB()
	hbWG.Wait()

	if err == nil {
		if cerr := w.markComplete(ctx, r.id); cerr != nil {
			w.cfg.Logger.Warn("jobs markComplete", "row", r.id, "err", cerr)
		}
		if w.completeHook != nil {
			w.completeHook.OnJobComplete(ctx, r.id)
		}
		return
	}
	w.reportFailure(ctx, r, err)
}

// markComplete writes the terminal state for a successful job.
// Wrapped in its own tx — the handler's tx (if any) already
// committed, so this is a discrete "I'm done" write.
func (w *Worker) markComplete(ctx context.Context, id int64) error {
	_, err := w.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET status        = 'complete',
       completed_at  = now(),
       claimed_by    = NULL,
       claimed_until = NULL
 WHERE id = $1`, id)
	return err
}

// heartbeat extends the lease so a peer doesn't poach this row.
// Idempotent: a duplicate heartbeat is a no-op write.
func (w *Worker) heartbeat(ctx context.Context, id int64) error {
	_, err := w.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET claimed_until = now() + ($1 || ' milliseconds')::interval
 WHERE id = $2 AND claimed_by = $3`,
		w.cfg.HeartbeatBudget.Milliseconds(), id, w.cfg.PodID)
	return err
}

// reportFailure records a handler error. If attempts exceeded
// max_retries, moves the row to atlantis.jobs_dead; otherwise resets
// status to pending so the next drain pass retries.
//
// All-in-one tx: a torn move (deleted from jobs but not inserted
// into jobs_dead, or vice versa) would leak rows. The tx wraps the
// INSERT + DELETE atomically.
func (w *Worker) reportFailure(ctx context.Context, r claimedRow, handlerErr error) {
	msg := handlerErr.Error()
	if r.attempts >= r.maxRetries {
		if err := w.moveToDLQ(ctx, r.id, msg); err != nil {
			w.cfg.Logger.Error("jobs moveToDLQ", "row", r.id, "err", err)
		}
		if w.completeHook != nil {
			w.completeHook.OnJobFailed(ctx, r.id, msg)
		}
		return
	}
	_, err := w.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET status        = 'pending',
       last_error    = $1,
       last_error_at = now(),
       claimed_by    = NULL,
       claimed_until = NULL
 WHERE id = $2`, msg, r.id)
	if err != nil {
		w.cfg.Logger.Warn("jobs reportFailure", "row", r.id, "err", err)
	}
}

// reportTransientFailure handles errors the operator can fix at
// runtime without a code change — currently only the missing-handler
// case. We bump attempts so a persistently-missing handler eventually
// DLQ's, but we keep the row in `running` until the lease expires so
// a peer pod with the handler can claim it.
func (w *Worker) reportTransientFailure(ctx context.Context, r claimedRow, err error) {
	if r.attempts >= r.maxRetries {
		_ = w.moveToDLQ(ctx, r.id, err.Error())
		return
	}
	_, qerr := w.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET last_error    = $1,
       last_error_at = now()
 WHERE id = $2`, err.Error(), r.id)
	if qerr != nil {
		w.cfg.Logger.Warn("jobs reportTransientFailure", "row", r.id, "err", qerr)
	}
}

// moveToDLQ atomically removes a row from atlantis.jobs and inserts
// the matching row into atlantis.jobs_dead. The INSERT preserves the
// row id so a subsequent RetryDeadJob can route by id.
func (w *Worker) moveToDLQ(ctx context.Context, id int64, errMsg string) error {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.jobs_dead
    (id, job_name, queue, args, attempts, max_retries, last_error, last_error_at, enqueued_at, submitted_by)
SELECT id, job_name, queue, args, attempts, max_retries, $2, now(), enqueued_at, submitted_by
FROM atlantis.jobs WHERE id = $1`, id, errMsg); err != nil {
		return fmt.Errorf("insert dead: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM atlantis.jobs WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete jobs: %w", err)
	}
	return tx.Commit(ctx)
}
