package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// checkpointerKey is the ctx key the worker uses to stash a
// per-claim Checkpointer for the handler. Private — handlers
// reach it through the exported Checkpoint function rather than
// pulling the key out of context themselves.
type checkpointerKey struct{}

// Checkpointer writes progress updates against atlantis.jobs for one
// in-flight claim. Two implementations: the direct-PG Worker installs
// a pgCheckpointer that runs an UPDATE against the pool; the
// DispatchedWorker installs a streamCheckpointer that emits a
// CheckpointMsg envelope over the gRPC control channel.
//
// Writes are best-effort: a failed Report doesn't fail the handler.
// The progress columns are advisory, not part of the job's commit
// semantics; the worker's terminal Complete / Fail signals are the
// source of truth for whether the job ran.
type Checkpointer interface {
	Report(ctx context.Context, pct int, msg string) error
}

// pgCheckpointer is the direct-PG implementation used by the
// SDK Worker (clients/go/jobs/runner.go). Hits atlantis.jobs from
// the worker's own pool — assumes the worker has PG creds.
type pgCheckpointer struct {
	pool  *pgxpool.Pool
	jobID int64
}

// newCheckpointer constructs the direct-PG flavor of Checkpointer.
// Called by the direct-PG Worker before handler dispatch. The
// DispatchedWorker uses newStreamCheckpointer instead.
func newCheckpointer(pool *pgxpool.Pool, jobID int64) Checkpointer {
	return &pgCheckpointer{pool: pool, jobID: jobID}
}

// Report writes a progress snapshot to atlantis.jobs. pct must be
// 0..100; the function clamps out-of-range values rather than
// erroring so a sloppy handler call doesn't fail an otherwise-
// healthy job. msg is a free-form short label the operator sees
// in `tide job status`.
func (c *pgCheckpointer) Report(ctx context.Context, pct int, msg string) error {
	if c == nil || c.pool == nil {
		return errors.New("jobs: nil checkpointer")
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	_, err := c.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET progress_pct = $1,
       progress_msg = $2,
       progress_at  = now()
 WHERE id = $3`, pct, msg, c.jobID)
	if err != nil {
		return fmt.Errorf("jobs: checkpoint: %w", err)
	}
	return nil
}

// Checkpoint reports per-claim progress to the worker. Handlers call
// this from inside their Handle method; the context must be the one
// the worker passed in (a fresh context.Background() won't carry the
// claim's Checkpointer).
//
// Returns nil when no checkpointer is attached, so the handler can
// call Checkpoint safely in unit tests that don't run through the
// worker. The "no-op when not running under the worker" semantic
// is the same as OpenTelemetry's no-op tracer when no provider is
// configured.
func Checkpoint(ctx context.Context, pct int, msg string) error {
	c, ok := ctx.Value(checkpointerKey{}).(Checkpointer)
	if !ok || c == nil {
		return nil
	}
	return c.Report(ctx, pct, msg)
}

// withCheckpointer returns ctx wrapped with a Checkpointer the
// handler can retrieve via Checkpoint. Internal to the worker.
func withCheckpointer(ctx context.Context, c Checkpointer) context.Context {
	return context.WithValue(ctx, checkpointerKey{}, c)
}
