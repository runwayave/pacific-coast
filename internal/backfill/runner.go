package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/obs"
)

// Config tunes the worker.
type Config struct {
	// Schema overrides the default "atlantis" schema name.
	Schema string

	// PollInterval is how often the worker polls for pending fields when
	// idle. Default 1s.
	PollInterval time.Duration

	// ChunkSize is the LIMIT on each chunked UPDATE. Default 10000.
	ChunkSize int

	// Throttle is the gap between successive chunks on the same field —
	// bounds replication lag and pg load. Default 100ms.
	Throttle time.Duration

	// Logger receives structured events. Defaults to slog.Default.
	Logger *slog.Logger
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		Schema:       "atlantis",
		PollInterval: time.Second,
		ChunkSize:    10000,
		Throttle:     100 * time.Millisecond,
	}
}

// Worker drains backfill_field_state rows. Multiple pods running the
// worker coexist safely: row claims use FOR UPDATE SKIP LOCKED; Phase-3
// transition uses a CAS on backfill_plan.status.
type Worker struct {
	pool *pgxpool.Pool
	cfg  Config

	lastDrainNS atomic.Int64
}

// NewWorker constructs a Worker; defaults are applied for zero-valued
// fields. The returned worker is idle until Run is called.
func NewWorker(pool *pgxpool.Pool, cfg Config) *Worker {
	if cfg.Schema == "" {
		cfg.Schema = "atlantis"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 10000
	}
	if cfg.Throttle < 0 {
		cfg.Throttle = 100 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Worker{pool: pool, cfg: cfg}
	w.lastDrainNS.Store(time.Now().UnixNano())
	return w
}

// Run blocks until ctx cancels. Each tick: try to drain one pending
// chunk; if none pending, try to advance a complete plan into Phase 3.
func (w *Worker) Run(ctx context.Context) error {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("backfill worker panic", "panic", rec)
		}
	}()
	t := time.NewTicker(w.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// LastDrainAt is the worker heartbeat, surfaced via /readyz.
func (w *Worker) LastDrainAt() time.Time {
	return time.Unix(0, w.lastDrainNS.Load())
}

func (w *Worker) tick(ctx context.Context) {
	w.lastDrainNS.Store(time.Now().UnixNano())
	w.updatePlansInFlightGauge(ctx)

	did, err := w.drainOnePendingField(ctx)
	if err != nil {
		w.cfg.Logger.Warn("backfill drain", "err", err)
		return
	}
	if did {
		// A field was claimed and processed — return so the next tick
		// has fresh ctx + heartbeat. If more work exists, the next tick
		// picks it up.
		return
	}
	if err := w.tryPhaseThreeOnce(ctx); err != nil {
		w.cfg.Logger.Warn("backfill phase3", "err", err)
	}
}

type pendingField struct {
	PlanHash      string
	EntityID      string
	Field         string
	Expression    string
	PKColumn      string
	TableName     string // schema-qualified, pre-quoted
	LastPK        int64  // 0 sentinel for "not started"
	RowsProcessed int64
}

// drainOnePendingField claims one row from backfill_field_state and runs
// one chunked UPDATE. Returns (did_work, err). All work is inside a
// single tx so a pod crash either commits both the UPDATE and the cursor
// advance, or neither.
func (w *Worker) drainOnePendingField(ctx context.Context) (bool, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	claimSQL := fmt.Sprintf(`
SELECT f.plan_hash, f.entity_id, f.field, f.expression, f.pk_column, f.table_name,
       COALESCE(f.last_pk::bigint, 0), f.rows_processed
FROM %s.backfill_field_state f
JOIN %s.backfill_plan p ON p.plan_hash = f.plan_hash
WHERE p.status = 'phase2_running'
  AND f.status IN ('pending','running')
ORDER BY f.started_at NULLS FIRST, f.plan_hash, f.entity_id, f.field
LIMIT 1
FOR UPDATE OF f SKIP LOCKED`, w.cfg.Schema, w.cfg.Schema)

	var pf pendingField
	err = tx.QueryRow(ctx, claimSQL).Scan(
		&pf.PlanHash, &pf.EntityID, &pf.Field, &pf.Expression,
		&pf.PKColumn, &pf.TableName, &pf.LastPK, &pf.RowsProcessed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}

	// Mark running (still inside the row lock).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_field_state
SET status='running', started_at = COALESCE(started_at, now())
WHERE plan_hash=$1 AND entity_id=$2 AND field=$3`, w.cfg.Schema),
		pf.PlanHash, pf.EntityID, pf.Field); err != nil {
		return true, fmt.Errorf("mark running: %w", err)
	}

	chunkSQL := ChunkSQL(pf.TableName, pf.PKColumn, pf.Field, pf.Expression)
	start := time.Now()
	var newLastPK int64
	var rowsUpdated int64
	err = tx.QueryRow(ctx, chunkSQL, pf.LastPK, w.cfg.ChunkSize).Scan(&newLastPK, &rowsUpdated)
	if err != nil {
		// The chunk errored — typically a malformed user expression or
		// a column reference that doesn't exist. Postgres puts the tx
		// into an aborted state, so any further statement on `tx` (the
		// markFieldFailed UPDATE, the Commit) is silently ignored. Roll
		// back explicitly and use a fresh pool exec so the field flips
		// to 'failed' and the worker stops re-claiming it.
		_ = tx.Rollback(ctx)
		w.markFieldFailedPool(ctx, pf, err)
		obs.BackfillChunksProcessed.WithLabelValues(truncateHash(pf.PlanHash), pf.EntityID, pf.Field, "failure").Inc()
		return true, fmt.Errorf("chunk: %w", err)
	}
	obs.BackfillChunkDuration.WithLabelValues(pf.EntityID, pf.Field).Observe(time.Since(start).Seconds())

	// Determine if this is the final chunk (no rows updated).
	newStatus := "running"
	if rowsUpdated == 0 {
		newStatus = "complete"
	}
	// last_pk on backfill_field_state is TEXT for portability across PK
	// types — bind it as the string form, not the int64. pgx refuses to
	// encode an int64 into a text-typed parameter even with `::text`
	// in the SQL.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_field_state
SET last_pk=$1,
    rows_processed = rows_processed + $2,
    status = $3,
    completed_at = CASE WHEN $3 = 'complete' THEN now() ELSE completed_at END
WHERE plan_hash=$4 AND entity_id=$5 AND field=$6`, w.cfg.Schema),
		strconv.FormatInt(newLastPK, 10), rowsUpdated, newStatus, pf.PlanHash, pf.EntityID, pf.Field); err != nil {
		return true, fmt.Errorf("advance cursor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit: %w", err)
	}

	obs.BackfillChunksProcessed.WithLabelValues(truncateHash(pf.PlanHash), pf.EntityID, pf.Field, "success").Inc()
	obs.BackfillRowsProcessed.WithLabelValues(truncateHash(pf.PlanHash), pf.EntityID, pf.Field).Add(float64(rowsUpdated))

	// Throttle between chunks of the same field. Empty chunks (final
	// pass) don't throttle since we're done with this field.
	if rowsUpdated > 0 && w.cfg.Throttle > 0 {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(w.cfg.Throttle):
		}
	}
	return true, nil
}

// markFieldFailedPool flips the field to 'failed' via the pool (a fresh
// implicit tx). Used when the chunk's own tx has aborted and is no
// longer usable for any further statements.
func (w *Worker) markFieldFailedPool(ctx context.Context, pf pendingField, applyErr error) {
	msg := sanitizeError(applyErr)
	if _, err := w.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_field_state
SET status='failed', error_msg=$1, completed_at=now()
WHERE plan_hash=$2 AND entity_id=$3 AND field=$4`, w.cfg.Schema),
		msg, pf.PlanHash, pf.EntityID, pf.Field); err != nil {
		w.cfg.Logger.Error("mark field failed", "plan", pf.PlanHash, "field", pf.Field, "err", err)
	}
}

// tryPhaseThreeOnce looks for a plan where every field is complete and
// runs Phase 3: SET NOT NULL on the deferred fields + ir_checkpoint
// validation. CAS on backfill_plan.status ensures only one pod runs
// Phase 3 per plan.
func (w *Worker) tryPhaseThreeOnce(ctx context.Context) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	findSQL := fmt.Sprintf(`
SELECT p.plan_hash, p.post_sql, p.post_indexes_sql, p.ir_checkpoint_hash
FROM %s.backfill_plan p
WHERE p.status = 'phase2_running'
  AND NOT EXISTS (
    SELECT 1 FROM %s.backfill_field_state f
    WHERE f.plan_hash = p.plan_hash AND f.status != 'complete'
  )
LIMIT 1
FOR UPDATE SKIP LOCKED`, w.cfg.Schema, w.cfg.Schema)

	var planHash, postSQL, postIndexesSQL, expectedIRHash string
	err = tx.QueryRow(ctx, findSQL).Scan(&planHash, &postSQL, &postIndexesSQL, &expectedIRHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find ready plan: %w", err)
	}

	// CAS to phase3_running (still under the row lock).
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_plan
SET status='phase3_running'
WHERE plan_hash=$1 AND status='phase2_running'`, w.cfg.Schema), planHash)
	if err != nil {
		return fmt.Errorf("cas phase3: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another pod beat us — nothing to do.
		return nil
	}

	// Validate ir_checkpoint hasn't drifted. If it has, fail the plan;
	// the operator gets to triage manually rather than auto-corrupting.
	currentHash, hashErr := currentIRCheckpointHash(ctx, tx, w.cfg.Schema)
	if hashErr == nil && currentHash != expectedIRHash {
		w.markPlanFailed(ctx, tx, planHash, fmt.Errorf("ir_checkpoint shifted under us (expected=%s got=%s)", expectedIRHash, currentHash))
		_ = tx.Commit(ctx)
		return fmt.Errorf("ir_checkpoint drift for plan %s", planHash)
	}

	if _, err := tx.Exec(ctx, postSQL); err != nil {
		w.markPlanFailed(ctx, tx, planHash, err)
		_ = tx.Commit(ctx)
		return fmt.Errorf("post_sql: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_plan
SET status='complete', completed_at=now()
WHERE plan_hash=$1`, w.cfg.Schema), planHash); err != nil {
		return fmt.Errorf("mark complete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit phase3: %w", err)
	}

	// Outside the tx: DROP INDEX CONCURRENTLY can't run inside a
	// transaction. Best-effort; index leak is recoverable manually.
	for _, stmt := range splitStatements(postIndexesSQL) {
		if _, err := w.pool.Exec(ctx, stmt); err != nil {
			w.cfg.Logger.Warn("backfill post-index drop", "plan", planHash, "stmt", stmt, "err", err)
		}
	}
	w.cfg.Logger.Info("backfill plan complete", "plan_hash", planHash)
	return nil
}

func (w *Worker) markPlanFailed(ctx context.Context, tx pgx.Tx, planHash string, err error) {
	msg := sanitizeError(err)
	_, _ = tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.backfill_plan
SET status='failed', error_msg=$1, completed_at=now()
WHERE plan_hash=$2`, w.cfg.Schema), msg, planHash)
}

func (w *Worker) updatePlansInFlightGauge(ctx context.Context) {
	q := fmt.Sprintf(`SELECT status, COUNT(*) FROM %s.backfill_plan WHERE status IN ('phase2_running','phase3_running') GROUP BY status`, w.cfg.Schema)
	rows, err := w.pool.Query(ctx, q)
	if err != nil {
		return
	}
	defer rows.Close()
	counts := map[string]float64{"phase2_running": 0, "phase3_running": 0}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return
		}
		counts[status] = float64(n)
	}
	for status, count := range counts {
		obs.BackfillPlansInFlight.WithLabelValues(status).Set(count)
	}
}

// currentIRCheckpointHash returns sha256 hex of the current ir_checkpoint
// JSONB bytes. Used at Phase 3 to detect IR drift since Phase 1.
func currentIRCheckpointHash(ctx context.Context, tx pgx.Tx, schema string) (string, error) {
	var raw []byte
	q := fmt.Sprintf(`SELECT ir::text FROM %s.ir_checkpoint WHERE id=1`, schema)
	if err := tx.QueryRow(ctx, q).Scan(&raw); err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// IRCheckpointHash is the package-public form of currentIRCheckpointHash
// used by the admin RPC at Phase 1 to capture the hash that Phase 3 will
// validate against.
func IRCheckpointHash(ctx context.Context, tx pgx.Tx, schema string) (string, error) {
	return currentIRCheckpointHash(ctx, tx, schema)
}

// sanitizeError trims a raw error down to a category string. Mirrors
// the invalidate worker's sanitizeError — pg errors carry hostnames /
// query fragments / PII that we don't want persisted in error_msg.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	// Surface the first line only, truncated. Operators get enough to
	// triage; PII / connection strings stay out of the table.
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// splitStatements breaks a multi-line SQL script into individual
// statements (one per non-blank, non-comment line). Used for
// CREATE/DROP INDEX CONCURRENTLY blocks that can't run inside a tx.
func splitStatements(script string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// truncateHash bounds plan_hash label cardinality. 8 hex chars = 4B
// distinct values, more than enough for any practical operator pool.
func truncateHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
