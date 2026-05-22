// Package auth implements caller-identity enforcement for the atlantis
// gRPC surface. The allowlist is loaded from atlantis.caller_registrations
// on startup and refreshed on a periodic ticker — new callers become
// callable within a refresh interval of their first admin.ApplyMigration.
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CallerAllowlist is a snapshot of distinct callers registered via
// atlantis.caller_registrations. The auth interceptor checks every
// non-exempt RPC against this set. Reload swaps the set atomically;
// readers never see a partial state.
type CallerAllowlist struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu  sync.RWMutex
	set map[string]struct{}
}

// New returns an empty allowlist bound to pool. Call Reload before
// gating any RPCs against it — a fresh-process Allows() returns false
// for everything until the first successful Reload.
func New(pool *pgxpool.Pool, log *slog.Logger) *CallerAllowlist {
	if log == nil {
		log = slog.Default()
	}
	return &CallerAllowlist{
		pool: pool,
		log:  log,
		set:  map[string]struct{}{},
	}
}

// Reload reads the full set of distinct callers from
// atlantis.caller_registrations and swaps it in atomically. Errors
// propagate so the caller can decide whether to abort startup or log
// and continue with the previous snapshot.
func (a *CallerAllowlist) Reload(ctx context.Context) error {
	rows, err := a.pool.Query(ctx, `SELECT DISTINCT caller FROM atlantis.caller_registrations`)
	if err != nil {
		return fmt.Errorf("auth: query caller_registrations: %w", err)
	}
	defer rows.Close()
	fresh := map[string]struct{}{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return fmt.Errorf("auth: scan caller: %w", err)
		}
		fresh[c] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	a.set = fresh
	a.mu.Unlock()
	return nil
}

// Allows reports whether caller appears in the most recent snapshot.
func (a *CallerAllowlist) Allows(caller string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.set[caller]
	return ok
}

// Size returns the current snapshot size — useful for /readyz, logs,
// and tests.
func (a *CallerAllowlist) Size() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.set)
}

// RunRefresher reloads the allowlist on a fixed interval until ctx
// cancels. Errors log at WARN; the previous snapshot keeps serving so
// a transient PG blip can't lock every caller out of the gRPC surface.
// interval <= 0 falls back to 30s.
func (a *CallerAllowlist) RunRefresher(ctx context.Context, interval time.Duration) {
	defer func() {
		if rec := recover(); rec != nil {
			a.log.Error("allowlist refresher panic", "panic", rec)
		}
	}()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.Reload(ctx); err != nil {
				a.log.Warn("allowlist reload", "err", err)
			}
		}
	}
}
