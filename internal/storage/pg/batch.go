package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Batch wraps pgx.Batch with a small ergonomic shell.
//
// Why this exists: per-row inserts in bulk paths (e.g. 5k-row imports
// issuing 5k round-trips) need a single batched round trip. Generated
// batch-insert handlers use this wrapper so they never have to know
// whether the underlying driver is pgx, some future pq, or a fake in tests.
//
// Usage:
//
//	b := pg.NewBatch()
//	for _, row := range rows {
//	    b.Queue("INSERT INTO ... VALUES ($1, $2)", row.A, row.B)
//	}
//	if err := pool.SendBatch(ctx, b); err != nil { ... }
//
// All queued statements run on a single connection inside one round trip.
type Batch struct {
	inner *pgx.Batch
}

// NewBatch returns an empty Batch ready to accept Queue calls.
func NewBatch() *Batch { return &Batch{inner: &pgx.Batch{}} }

// Queue appends a parameterized statement to the batch. Position in the
// batch matters; results come back in the same order.
func (b *Batch) Queue(sql string, args ...any) { b.inner.Queue(sql, args...) }

// Len returns how many statements are queued. Used by callers that want to
// decide between SendBatch and a single multi-row INSERT.
func (b *Batch) Len() int { return b.inner.Len() }

// SendBatch dispatches a batch on the pool. It iterates results and drains
// any error. Callers that need the per-statement Result (e.g. RETURNING
// values) should reach for SendBatchResults instead.
//
// The default behavior is fire-and-check: every queued Exec or Query must
// succeed; the first error short-circuits the rest. Generated code uses this
// for bulk inserts where the caller only needs "all-or-nothing" semantics.
func (p *Pool) SendBatch(ctx context.Context, b *Batch) error {
	if b == nil || b.Len() == 0 {
		return nil
	}
	res := p.pool.SendBatch(ctx, b.inner)
	defer func() { _ = res.Close() }()
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("batch statement: %w", err)
		}
	}
	return nil
}

// SendBatchResults gives the caller raw access to the pgx.BatchResults so
// they can pull RETURNING values one statement at a time. Use only when you
// genuinely need each statement's response — most code wants SendBatch.
func (p *Pool) SendBatchResults(ctx context.Context, b *Batch) pgx.BatchResults {
	return p.pool.SendBatch(ctx, b.inner)
}

// RunInTx executes fn inside a transaction. It begins, runs fn, and commits
// if fn returns nil; otherwise it rolls back. The same context is threaded
// through so query deadlines flow uniformly.
//
// This matches the pattern the generated server handlers use today, but
// centralizes the begin/defer/commit dance so future callers (e.g., the
// outbox sweeper, integration tests) don't reinvent it.
//
// Invariant the codegen relies on: cache invalidation work happens INSIDE
// the tx (via runtime.Outbox.Enqueue) so the invalidation outbox row is
// written atomically with the data write. Anything that needs to run AFTER
// commit must do so outside fn — RunInTx itself does no post-commit work.
func (p *Pool) RunInTx(ctx context.Context, fn func(ctx context.Context, tx runtime.Tx) error) error {
	tx, err := p.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Best-effort rollback: pgx returns ErrTxClosed after Commit; we discard
	// that specific error because it's expected.
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
