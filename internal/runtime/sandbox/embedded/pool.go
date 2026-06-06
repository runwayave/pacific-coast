package embedded

// Local runtime.Pool implementation for the embedded backend. We
// can't reuse internal/storage/pg directly because that wrapper's
// AfterConnect hook registers pgvector types — and the vanilla
// Postgres bundled by fergusstrange/embedded-postgres doesn't ship
// the vector extension. The embedded backend documents that:
// vector queries route to the sim, custom SQL routes here.
//
// The wire shape is identical to internal/storage/pg's adapter: same
// runtime.Pool / runtime.Tx / runtime.Row / runtime.Rows / runtime.CommandTag
// interfaces, same no-rows wrapping. Generated handlers can't tell
// which adapter sits underneath.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// localPool wraps a *pgxpool.Pool and satisfies runtime.Pool without
// requiring pgvector registration. Mirrors internal/storage/pg.Pool's
// shape so the codegen-emitted scan and bind paths work unchanged.
type localPool struct {
	pool *pgxpool.Pool
}

// newLocalPool constructs a pgxpool against the embedded PG, skipping
// the pgvector AfterConnect hook.
func newLocalPool(ctx context.Context, url string) (*localPool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// UTC timezone so timestamptz round-trips don't surprise
		// callers running in non-UTC machines. Same convention as
		// internal/storage/pg.
		if _, err := conn.Exec(ctx, "SET TIME ZONE 'UTC'"); err != nil {
			return fmt.Errorf("set timezone: %w", err)
		}
		// Deliberately no pgvector registration — the embedded PG
		// doesn't have the extension. Vector queries belong to the
		// sim backend.
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &localPool{pool: pool}, nil
}

// Close releases the pool.
func (p *localPool) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

// QueryRow runs a single-row query.
func (p *localPool) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return localRow{row: p.pool.QueryRow(ctx, sql, args...)}
}

// Query runs a multi-row query.
func (p *localPool) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	r, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return localRows{rows: r}, nil
}

// Exec runs a non-returning statement.
func (p *localPool) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	t, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return localCommandTag{tag: t}, nil
}

// BeginTx starts a tx with default isolation. Caller invokes Commit
// or Rollback exactly once.
func (p *localPool) BeginTx(ctx context.Context) (runtime.Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &localTx{tx: tx}, nil
}

// Compile-time check.
var _ runtime.Pool = (*localPool)(nil)

type localRow struct {
	row pgx.Row
}

// Scan passes the pgx error through verbatim. runtime.IsNoRows
// matches by literal string equality on `err.Error()`, and pgx's
// ErrNoRows formats to "no rows in result set" — so generated
// handlers get the no-rows signal without any wrapping on this side.
// Wrapping with %w would prepend a prefix and break the literal
// match (the runtime.IsNoRows literal-string check).
func (r localRow) Scan(dest ...any) error {
	return r.row.Scan(dest...)
}

type localRows struct {
	rows pgx.Rows
}

func (r localRows) Next() bool             { return r.rows.Next() }
func (r localRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r localRows) Err() error             { return r.rows.Err() }
func (r localRows) Close()                 { r.rows.Close() }

type localCommandTag struct {
	tag pgconn.CommandTag
}

func (t localCommandTag) RowsAffected() int64 { return t.tag.RowsAffected() }

type localTx struct {
	tx pgx.Tx
}

func (t *localTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return localRow{row: t.tx.QueryRow(ctx, sql, args...)}
}

func (t *localTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	r, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return localRows{rows: r}, nil
}

func (t *localTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return localCommandTag{tag: tag}, nil
}

func (t *localTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *localTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }
