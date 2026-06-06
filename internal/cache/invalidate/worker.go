package invalidate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/obs"
)

// VersionSetter is the slice of the memcached client the worker needs.
// internal/cache/memcached.*Client satisfies this; the small interface
// keeps the worker package free of memcache-specific imports.
//
// IsStale lets the worker distinguish "we lost the race against another
// worker — row is done" from a real apply failure. The memcached client
// returns its ErrStaleVersion sentinel; this method wraps the check so the
// worker doesn't need to import that sentinel directly.
type VersionSetter interface {
	SetVersion(ctx context.Context, entity, id string, version int64, ttl time.Duration) error
	IsStale(err error) bool
}

// LRUInvalidator is the in-process LRU notification surface. The reader
// package's *Reader satisfies this. After memcached SET succeeds we tell
// every running reader to drop its tier-0 entry; without that, tier-0
// would happily keep serving stale bytes until eviction.
type LRUInvalidator interface {
	Invalidate(entity, id string)
}

// GenerationBumper increments the per-entity counter that keys the
// tier-2 query-result cache. queryresult.*Cache satisfies this; the
// narrow interface keeps the invalidate package decoupled from the
// query-result cache implementation.
//
// One bump invalidates every cached query result for the entity at
// once. Bursts of writes get coalesced by the worker's per-entity
// debouncer so memcached doesn't see one counter increment per row.
type GenerationBumper interface {
	BumpGeneration(ctx context.Context, entity string) (int64, error)
}

// WorkerConfig tunes the drain loop.
type WorkerConfig struct {
	// Schema overrides the default "atlantis" schema name.
	Schema string

	// DrainInterval is the periodic wake-up interval; the worker also wakes
	// on LISTEN/NOTIFY. Default 250ms.
	DrainInterval time.Duration

	// BatchSize is the max rows drained per loop. Default 100.
	BatchSize int

	// PointerTTL is the TTL on the version-pointer key when SET in memcached.
	// Long enough that an offline reader cluster catches up after restart.
	// Default 24h.
	PointerTTL time.Duration

	// AlertLag is the threshold over which the sweeper logs at WARN that the
	// worker is behind. Default 5m.
	AlertLag time.Duration

	// BumpDebounce is the minimum gap between successive
	// BumpGeneration calls for the same entity. A burst of writes to
	// one entity within this window collapses to a single bump.
	// Default 100ms.
	BumpDebounce time.Duration

	// MaxAttempts caps per-row retries. A row that hits MaxAttempts
	// is moved to cache_invalidations_dead so subsequent drain passes
	// don't keep claiming it. Default 100.
	MaxAttempts int

	// Logger receives structured events. Defaults to slog.Default.
	Logger *slog.Logger
}

// DefaultWorkerConfig returns the default defaults.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Schema:        "atlantis",
		DrainInterval: 250 * time.Millisecond,
		BatchSize:     100,
		PointerTTL:    24 * time.Hour,
		AlertLag:      5 * time.Minute,
		BumpDebounce:  100 * time.Millisecond,
		MaxAttempts:   100,
	}
}

// Worker drains the cache_invalidations outbox.
//
// The drain loop:
//  1. Wake on LISTEN/NOTIFY ("atl_cache_invalidations") OR periodic timer.
//  2. SELECT the oldest batch of rows.
//  3. For each row: SetVersion in memcached, Invalidate the in-process LRU,
//     DELETE the outbox row. Failures bump `attempts` and leave the row in
//     place for the next loop.
//
// Worker safety: there can be multiple workers in different pods. The
// SELECT uses FOR UPDATE SKIP LOCKED so two workers never claim the same
// row, and per-row success is idempotent (memcached SET is idempotent, the
// DELETE removes the row exactly once across the cluster).
type Worker struct {
	pool *pgxpool.Pool
	mc   VersionSetter
	lru  LRUInvalidator
	qc   GenerationBumper
	cfg  WorkerConfig

	// lastBump tracks the most recent BumpGeneration call per entity.
	// Subsequent generation_bump rows for the same entity within
	// cfg.BumpDebounce are consumed without re-bumping. The mutex is
	// cheap relative to memcached round-trips.
	bumpMu   sync.Mutex
	lastBump map[string]time.Time

	stopped chan struct{}

	// lastDrainNS is the unix-nano time of the most recent successful
	// claim from the outbox table. Read by LastDrainAt for /readyz
	// heartbeat checks. Atomic so the readiness handler doesn't contend
	// with the drain goroutine.
	lastDrainNS atomic.Int64
}

// NewWorker constructs a Worker. The pool MUST be a pgxpool so we can use
// pgx's native LISTEN/NOTIFY via Acquire(). Returns an error iff
// cfg.Schema is not a valid SQL identifier — the worker's SQL is built
// with fmt.Sprintf so a hostile schema would otherwise produce injection.
//
// qc is optional. When nil, generation_bump rows are still drained but
// the bump is a no-op — this lets older callers (and tests that don't
// exercise tier-2 caching) keep working without wiring a query-cache.
func NewWorker(pool *pgxpool.Pool, mc VersionSetter, lru LRUInvalidator, qc GenerationBumper, cfg WorkerConfig) (*Worker, error) {
	if cfg.Schema == "" {
		cfg.Schema = "atlantis"
	}
	if err := validateSchema(cfg.Schema); err != nil {
		return nil, err
	}
	if cfg.DrainInterval == 0 {
		cfg.DrainInterval = 250 * time.Millisecond
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.PointerTTL == 0 {
		cfg.PointerTTL = 24 * time.Hour
	}
	if cfg.AlertLag == 0 {
		cfg.AlertLag = 5 * time.Minute
	}
	if cfg.BumpDebounce == 0 {
		cfg.BumpDebounce = 100 * time.Millisecond
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Worker{
		pool:     pool,
		mc:       mc,
		lru:      lru,
		qc:       qc,
		cfg:      cfg,
		lastBump: make(map[string]time.Time),
		stopped:  make(chan struct{}),
	}
	// Seed the heartbeat so /readyz survives the brief gap before the
	// first drain pass completes.
	w.lastDrainNS.Store(time.Now().UnixNano())
	return w, nil
}

// Run blocks until ctx is canceled. Drain errors are logged and retried;
// only ctx cancellation propagates up. The LISTEN session lives in its
// own goroutine that reconnects on any failure, so a transient pg drop
// degrades the worker to ticker-only polling instead of taking it offline.
//
// In production this runs in its own goroutine launched by cmd/server.
// In tests, run it in a goroutine and cancel ctx to stop.
func (w *Worker) Run(ctx context.Context) error {
	defer close(w.stopped)

	// First drain clears anything that landed before the LISTEN session
	// establishes. The ticker covers steady-state polling.
	w.drainOnce(ctx)

	ticker := time.NewTicker(w.cfg.DrainInterval)
	defer ticker.Stop()

	notifyCh := make(chan struct{}, 1)
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	go w.runListenLoop(listenCtx, notifyCh)
	go w.runLagPoller(ctx, 10*time.Second)

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

// runListenLoop opens LISTEN sessions on freshly-acquired pool
// connections, reconnecting after any session-ending error. Returns
// only when ctx is canceled. The 1s gap between sessions absorbs
// flapping connections without turning this into a hot loop.
func (w *Worker) runListenLoop(ctx context.Context, notifyCh chan struct{}) {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("listen loop panic", "panic", rec)
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.runListenSession(ctx, notifyCh)
		if ctx.Err() != nil {
			return
		}
		w.cfg.Logger.Warn("listen: session ended, reconnecting", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// runListenSession acquires a connection, registers LISTEN, and pushes
// wake signals into notifyCh until the session dies. The returned error
// is whatever WaitForNotification surfaced; runListenLoop re-checks ctx
// separately to distinguish shutdown from a real connection drop.
func (w *Worker) runListenSession(ctx context.Context, notifyCh chan struct{}) error {
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN atl_cache_invalidations"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case notifyCh <- struct{}{}:
		default:
		}
	}
}

// drainOnce processes up to BatchSize rows and returns the count
// claimed. Per-row failures are logged and skipped; the row stays in
// the outbox for the next iter. Drain reads the count to know when
// the table is empty.
//
// Concurrency invariant: claim + apply + delete are wrapped in
// a single Postgres transaction. `SELECT ... FOR UPDATE SKIP LOCKED` holds
// the row lock until COMMIT — without the tx, the lock was released the
// moment the SELECT returned, allowing another worker to claim and process
// the same row and produce a stale pointer overwrite. With the tx, only
// one worker can hold a given outbox row at a time across the full cycle.
//
// We do memcached SET *inside* the tx (an open Postgres tx wrapping a
// network call to memcached). Normally an anti-pattern, but here:
//
//   - the tx exists only to keep the row lock from claimInTx held until
//     DELETE — two workers may run their own txs in parallel against
//     different batches (SKIP LOCKED makes them disjoint), so there is
//     no global serialization being held open
//   - the memcached call has a short hard timeout (100ms by default)
//   - the tx only holds row locks on the small bounded batch we just
//     claimed; it does not block any data path
//
// Releasing the lock the moment the SELECT returns and then racing the
// DELETE is the alternative, and it lets two workers double-process the
// same row and produce a stale pointer overwrite.
func (w *Worker) drainOnce(ctx context.Context) int {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.cfg.Logger.Warn("drain: begin", "err", err)
		return 0
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := w.claimInTx(ctx, tx)
	if err != nil {
		w.cfg.Logger.Warn("drain: claim", "err", err)
		return 0
	}
	w.lastDrainNS.Store(time.Now().UnixNano())
	if len(rows) == 0 {
		return 0
	}

	for _, r := range rows {
		w.processRow(ctx, tx, r)
	}
	if err := tx.Commit(ctx); err != nil {
		w.cfg.Logger.Warn("drain: commit", "err", err)
	}
	return len(rows)
}

// Drain processes outbox rows until the table is empty or ctx expires.
// Called during graceful shutdown after Run returns. FOR UPDATE SKIP
// LOCKED makes it safe to call concurrently with Run, but pointless.
func (w *Worker) Drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w.drainOnce(ctx) == 0 {
			return nil
		}
	}
}

// LastDrainAt returns the time of the most recent successful claim from
// the outbox table. /readyz reads this as a worker heartbeat — a value
// older than the configured staleness threshold means the goroutine is
// wedged on something memcached, pg, or schedule-related.
func (w *Worker) LastDrainAt() time.Time {
	return time.Unix(0, w.lastDrainNS.Load())
}

// processRow handles one outbox row inside the drain tx. Wrapping apply
// in defer-recover keeps a panic on a single poisoned row from killing
// the worker; the panic logs, the row's attempts increment, and the
// next row in the batch runs. Both error and panic paths route through
// markFailureInTx, which moves attempts-exceeded rows to the dead-letter
// table so the main outbox stays small. The single deferred metrics
// increment covers all three exit paths (success / failure / panic).
func (w *Worker) processRow(ctx context.Context, tx pgx.Tx, r pendingRow) {
	kind := obs.NormalizeOutboxKind(r.Kind)
	result := "success"
	defer func() {
		if rec := recover(); rec != nil {
			result = "panic"
			obs.OutboxProcessed.WithLabelValues(kind, result).Inc()
			w.cfg.Logger.Error("drain: row panic",
				"id", r.ID, "kind", r.Kind, "entity", r.Entity, "row", r.RowID, "panic", rec)
			w.markFailureInTx(ctx, tx, r, fmt.Errorf("panic: %v", rec))
			return
		}
		obs.OutboxProcessed.WithLabelValues(kind, result).Inc()
	}()
	if err := w.apply(ctx, r); err != nil {
		result = "failure"
		w.cfg.Logger.Warn("drain: apply",
			"id", r.ID, "kind", r.Kind, "entity", r.Entity, "row", r.RowID, "err", err)
		w.markFailureInTx(ctx, tx, r, err)
		return
	}
	// LRU drop only matters for row-level invalidations. Generation
	// bumps invalidate the tier-2 query cache, which is not tier-0
	// LRU's concern.
	if w.lru != nil && (r.Kind == "" || r.Kind == "invalidation") {
		w.lru.Invalidate(r.Entity, r.RowID)
	}
	if err := w.deleteInTx(ctx, tx, r.ID); err != nil {
		w.cfg.Logger.Warn("drain: delete row", "id", r.ID, "err", err)
	}
}

// runLagPoller updates obs.OutboxLagSeconds on a coarse interval. The
// query is a full-table MIN over an indexed column on a small table —
// cheap, but not free-on-every-Prometheus-scrape, hence polling rather
// than a GaugeFunc. interval <= 0 falls back to 10s.
func (w *Worker) runLagPoller(ctx context.Context, interval time.Duration) {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("lag poller panic", "panic", rec)
		}
	}()
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.updateLagGauge(ctx)
		}
	}
}

func (w *Worker) updateLagGauge(ctx context.Context) {
	q := fmt.Sprintf(`SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(enqueued_at))), 0) FROM %s.cache_invalidations`, w.cfg.Schema)
	var lag float64
	if err := w.pool.QueryRow(ctx, q).Scan(&lag); err != nil {
		w.cfg.Logger.Debug("lag poll", "err", err)
		return
	}
	obs.OutboxLagSeconds.Set(lag)
}

type pendingRow struct {
	ID         int64
	Kind       string // "invalidation" or "generation_bump"
	Entity     string
	RowID      string
	NewVersion int64
	EnqueuedAt time.Time
	Attempts   int
}

// claimInTx pulls a batch of outbox rows inside the worker's transaction.
// `FOR UPDATE SKIP LOCKED` holds the row lock until the surrounding tx
// commits or rolls back — long enough for the DELETE that finishes the row.
//
// Rows in the per-row backoff window are skipped so a recently-failed
// row doesn't get hammered every drain tick. Backoff is exponential in
// attempts (base 100ms, capped at 1h); untried rows (last_error_at IS
// NULL) are always eligible.
func (w *Worker) claimInTx(ctx context.Context, tx pgx.Tx) ([]pendingRow, error) {
	q := fmt.Sprintf(`
SELECT id, kind, entity, row_id, new_version, enqueued_at, attempts
FROM %s.cache_invalidations
WHERE last_error_at IS NULL
   OR now() > last_error_at + LEAST(power(2, attempts) * interval '100 milliseconds', interval '1 hour')
ORDER BY enqueued_at
LIMIT $1
FOR UPDATE SKIP LOCKED`, w.cfg.Schema)
	rows, err := tx.Query(ctx, q, w.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Entity, &r.RowID, &r.NewVersion, &r.EnqueuedAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// apply dispatches a single outbox row by kind. The work it does is
// kind-specific, but the contract is the same: nil means the row was
// handled (the surrounding tx will DELETE it); a non-nil error means the
// row stays put and `attempts` increments.
//
// Lag alerting fires unconditionally because either kind of row stuck
// in the outbox indicates either the worker is wedged or memcached is
// down. We log here (not in the sweeper) so the first-time alert is
// immediate.
func (w *Worker) apply(ctx context.Context, r pendingRow) error {
	if age := time.Since(r.EnqueuedAt); age > w.cfg.AlertLag {
		w.cfg.Logger.Warn("outbox lag",
			"kind", r.Kind, "entity", r.Entity, "row", r.RowID, "age", age, "attempts", r.Attempts)
	}
	switch r.Kind {
	case "", "invalidation":
		// Empty string handles the (pre-migration / test) case where
		// the kind column hasn't been backfilled yet.
		return w.applyInvalidation(ctx, r)
	case "generation_bump":
		return w.applyGenerationBump(ctx, r)
	default:
		// Unknown kind = poisoned outbox row. Returning a non-nil
		// error keeps the row in place; the attempts counter will
		// climb until an operator intervenes. We deliberately don't
		// silently DELETE — corruption should be visible.
		return fmt.Errorf("unknown outbox kind %q", r.Kind)
	}
}

// applyInvalidation runs the memcached SET that publishes a row's new
// version pointer. The pointer TTL is long (default 24h); readers fall
// back to PG if they ever see no pointer.
//
// ErrStaleVersion (from the memcached client's monotonic guard) is NOT
// an error from the worker's perspective — it means another worker
// already landed a higher version. We delete the row and move on.
func (w *Worker) applyInvalidation(ctx context.Context, r pendingRow) error {
	err := w.mc.SetVersion(ctx, r.Entity, r.RowID, r.NewVersion, w.cfg.PointerTTL)
	if err != nil && w.mc.IsStale(err) {
		return nil
	}
	return err
}

// applyGenerationBump increments the per-entity counter that keys the
// tier-2 query-result cache, subject to per-entity debouncing.
//
// The debouncer collapses bursts of writes to one entity into a single
// counter increment. A burst of N writes within BumpDebounce produces
// one bump (covering all N) rather than N bumps; readers who issued a
// query result mid-burst may serve up to (TTL + BumpDebounce) stale
// state, which is well within the query-result cache's documented
// freshness model.
//
// When the worker was constructed without a GenerationBumper (older
// callers, tests that don't exercise tier-2), the bump is a no-op —
// the row still gets DELETEd so it doesn't pile up.
func (w *Worker) applyGenerationBump(ctx context.Context, r pendingRow) error {
	if w.qc == nil {
		return nil
	}
	if r.Entity == "" {
		return errors.New("generation_bump: empty entity")
	}

	w.bumpMu.Lock()
	last, seen := w.lastBump[r.Entity]
	if seen && time.Since(last) < w.cfg.BumpDebounce {
		w.bumpMu.Unlock()
		return nil
	}
	w.lastBump[r.Entity] = time.Now()
	w.bumpMu.Unlock()

	if _, err := w.qc.BumpGeneration(ctx, r.Entity); err != nil {
		// The bump failed; roll back our debounce record so a retry
		// isn't suppressed.
		w.bumpMu.Lock()
		if w.lastBump[r.Entity].Equal(last) || !seen {
			delete(w.lastBump, r.Entity)
		}
		w.bumpMu.Unlock()
		return err
	}
	return nil
}

// markFailureInTx bumps attempts and records last_error / last_error_at.
// Sanitized so we never persist raw error messages — those can leak
// hostnames, query fragments, or PII. We persist only the error *kind*
// and a truncated category.
//
// When the next attempt count meets MaxAttempts the row moves to the
// dead-letter table so subsequent drain passes don't keep claiming it.
// Operators inspect cache_invalidations_dead to triage poison rows.
func (w *Worker) markFailureInTx(ctx context.Context, tx pgx.Tx, r pendingRow, applyErr error) {
	msg := sanitizeError(applyErr)
	if r.Attempts+1 >= w.cfg.MaxAttempts {
		w.moveToDLQInTx(ctx, tx, r.ID, msg)
		return
	}
	q := fmt.Sprintf(`
UPDATE %s.cache_invalidations
SET attempts = attempts + 1, last_error = $2, last_error_at = now()
WHERE id = $1`, w.cfg.Schema)
	if _, err := tx.Exec(ctx, q, r.ID, msg); err != nil {
		w.cfg.Logger.Warn("mark failure", "id", r.ID, "err", err)
	}
}

// moveToDLQInTx atomically moves a poisoned outbox row to the dead-letter
// table inside the surrounding worker tx. The CTE-DELETE returns the
// original row; the INSERT preserves the original id, entity, etc.,
// bumps attempts to reflect this final try, and stamps the sanitized
// failure reason.
func (w *Worker) moveToDLQInTx(ctx context.Context, tx pgx.Tx, id int64, sanitizedErr string) {
	q := fmt.Sprintf(`
WITH moved AS (
    DELETE FROM %s.cache_invalidations WHERE id = $1 RETURNING *
)
INSERT INTO %s.cache_invalidations_dead
    (id, entity, row_id, new_version, kind, enqueued_at, attempts, last_error, last_error_at)
SELECT id, entity, row_id, new_version, kind, enqueued_at, attempts + 1, $2, now()
FROM moved`, w.cfg.Schema, w.cfg.Schema)
	if _, err := tx.Exec(ctx, q, id, sanitizedErr); err != nil {
		w.cfg.Logger.Error("dlq move failed", "id", id, "err", err)
		return
	}
	obs.OutboxDLQ.Inc()
	w.cfg.Logger.Warn("moved to dlq", "id", id, "err", sanitizedErr)
}

// deleteInTx removes a successfully-applied outbox row, inside the worker tx.
func (w *Worker) deleteInTx(ctx context.Context, tx pgx.Tx, id int64) error {
	q := fmt.Sprintf(`DELETE FROM %s.cache_invalidations WHERE id = $1`, w.cfg.Schema)
	_, err := tx.Exec(ctx, q, id)
	return err
}

// sanitizeError keeps a tiny category string so operators can grep, without
// persisting attacker-influenceable raw text. Categories pinned here so the
// audit story is "the only strings we ever store are these constants".
func sanitizeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	// Everything else is "apply_failed". We refuse to persist the raw
	// message because it can carry hostnames or query fragments.
	return "apply_failed"
}

// SweepOldRows is called periodically by the server (cmd/server) to remove
// settled rows older than 1h. Returns the count removed.
//
// Refuses to run with olderThan < 1 minute — that's almost certainly a
// misconfiguration and would delete in-flight outbox rows.
func (w *Worker) SweepOldRows(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan < time.Minute {
		return 0, fmt.Errorf("sweep: refusing olderThan=%v (< 1m would delete in-flight rows)", olderThan)
	}
	q := fmt.Sprintf(`
DELETE FROM %s.cache_invalidations
WHERE enqueued_at < now() - $1::interval`, w.cfg.Schema)
	tag, err := w.pool.Exec(ctx, q, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
