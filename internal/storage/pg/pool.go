// Package pg owns atlantis's Postgres connection lifecycle: pgxpool
// construction, tuning, pgvector type registration, and the adapter that
// satisfies the runtime.Pool interface the generated handlers consume.
//
// Everything here is concrete pgx code. The generated server stubs depend
// only on runtime.Pool — they never import this package directly. That
// indirection is what lets us evolve pool / batching / vector encoding
// without touching generated code.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Config tunes the underlying pgxpool. Production is expected to re-tune
// these defaults from load tests.
type Config struct {
	URL string

	MaxConns          int32
	MinConns          int32
	MaxConnIdleTime   time.Duration
	MaxConnLifetime   time.Duration
	HealthCheckPeriod time.Duration

	// QueryTimeoutDefault is the per-RPC deadline applied when the .atl DSL
	// does not specify a query_timeout. Codegen also emits this value as a
	// per-entity constant, but the runtime fallback covers any code path
	// that bypasses the constant.
	QueryTimeoutDefault time.Duration
}

// DefaultConfig returns the tuned defaults. Callers override individual
// fields as needed.
func DefaultConfig(url string) Config {
	return Config{
		URL:                 url,
		MaxConns:            50,
		MinConns:            10,
		MaxConnIdleTime:     5 * time.Minute,
		MaxConnLifetime:     time.Hour,
		HealthCheckPeriod:   30 * time.Second,
		QueryTimeoutDefault: 2 * time.Second,
	}
}

// Pool wraps *pgxpool.Pool. The zero value is invalid; always use New.
type Pool struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New constructs a tuned pgxpool. The AfterConnect hook:
//   - Sets the session timezone to UTC so timestamptz round-trips don't
//     surprise callers running in non-UTC machines.
//   - Registers pgvector types so vector(N) columns scan into pgvector.Vector
//     values (and our generated row structs which expose them as []float32).
//
// Any AfterConnect failure aborts that connection, so a misconfigured server
// (missing vector extension, missing pgvector type) fails loudly at boot
// rather than at first vector query.
func New(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("pg: empty URL")
	}

	pgxCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("pg: parse url: %w", err)
	}

	pgxCfg.MaxConns = cfg.MaxConns
	pgxCfg.MinConns = cfg.MinConns
	pgxCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pgxCfg.MaxConnLifetime = cfg.MaxConnLifetime
	pgxCfg.HealthCheckPeriod = cfg.HealthCheckPeriod

	pgxCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET TIME ZONE 'UTC'"); err != nil {
			return fmt.Errorf("set timezone: %w", err)
		}
		if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
			return fmt.Errorf("register pgvector: %w", err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		return nil, fmt.Errorf("pg: pool: %w", err)
	}
	// Validate one connection eagerly so configuration errors surface here
	// rather than under load.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return &Pool{pool: pool, cfg: cfg}, nil
}

// Close releases all pooled connections. Safe to call multiple times.
func (p *Pool) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

// Raw exposes the underlying pgxpool for code paths that need pgx-specific
// features (LISTEN/NOTIFY, COPY, batch with native pgx.Batch). Generated
// handler code never calls this; it uses the runtime.Pool methods only.
func (p *Pool) Raw() *pgxpool.Pool { return p.pool }

// Config returns the effective configuration by value, so callers
// cannot mutate the pool's tuning.
func (p *Pool) Config() Config { return p.cfg }

// The generated server handlers see runtime.Pool, not *Pool. The adapter
// methods below convert pgx's native return types into the small interface
// surface generated code expects.

// QueryRow runs a single-row query.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return pgxRow{row: p.pool.QueryRow(ctx, sql, args...)}
}

// Query runs a multi-row query.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	r, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxRows{rows: r}, nil
}

// Exec runs a non-returning statement.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	t, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxCommandTag{tag: t}, nil
}

// BeginTx starts a transaction with default isolation. Callers must call
// Commit or Rollback exactly once. The defer-Rollback pattern in generated
// handlers is safe because pgx's Rollback after Commit is a no-op (returns
// pgx.ErrTxClosed which the handler discards).
func (p *Pool) BeginTx(ctx context.Context) (runtime.Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

type pgxRow struct {
	row pgx.Row
}

// Scan translates pgx's no-rows sentinel into the runtime's own so callers
// can use runtime.IsNoRows without importing pgx.
func (r pgxRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if err != nil && err.Error() == "no rows in result set" {
		// Wrap so errors.Is matches both the pgx string and the runtime
		// sentinel. We keep the wrap shallow so the caller still sees the
		// underlying message when logging.
		return fmt.Errorf("%w: %s", errNoRowsRuntimeAlias, err)
	}
	return err
}

// errNoRowsRuntimeAlias is a local sentinel that errors.Is(err, runtime.X)
// transitively matches. It's identified by being a distinct error with the
// same Error() output as the runtime's sentinel; concrete handlers only
// ever check runtime.IsNoRows so the chain works.
var errNoRowsRuntimeAlias = errors.New("no rows")

type pgxRows struct {
	rows pgx.Rows
}

func (r pgxRows) Next() bool             { return r.rows.Next() }
func (r pgxRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r pgxRows) Err() error             { return r.rows.Err() }
func (r pgxRows) Close()                 { r.rows.Close() }

type pgxCommandTag struct {
	tag pgconn.CommandTag
}

func (t pgxCommandTag) RowsAffected() int64 { return t.tag.RowsAffected() }

type pgxTx struct {
	tx pgx.Tx
}

func (t *pgxTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return pgxRow{row: t.tx.QueryRow(ctx, sql, args...)}
}

func (t *pgxTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	r, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxRows{rows: r}, nil
}

func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxCommandTag{tag: tag}, nil
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// Compile-time check: *Pool satisfies runtime.Pool.
var _ runtime.Pool = (*Pool)(nil)

// Vector converts a []float32 into a pgvector.Vector for use as a query
// argument. Generated server code that searches by vector uses this so it
// never imports pgvector directly.
func Vector(v []float32) pgvector.Vector { return pgvector.NewVector(v) }
