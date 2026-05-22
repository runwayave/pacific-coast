//go:build integration

// Package integration runs end-to-end tests against a real Postgres +
// memcached. Build tag `integration` keeps Docker dependencies out of
// the default `go test ./...` pass — invoke with `make test-integration`.
//
// Architecture (PLAN.md §B.9 + §C.4):
//
//	Test process
//	  ├─ testcontainers Postgres (TimescaleDB image, same as docker-compose)
//	  ├─ testcontainers memcached (Grafana fork) — real wire protocol, real
//	  │   eviction. We use real memcached over an in-memory fake because
//	  │   PLAN §B.4's invalidation semantics depend on actual SET ordering;
//	  │   the in-memory fake stays available in fake_cache.go for unit-shape
//	  │   tests that don't exercise invalidation.
//	  └─ in-process atlantis (Embed): pool + reader + outbox + worker
//	      linked directly into the test binary, bypassing gRPC. The same
//	      runtime interfaces (Pool / Cache / Outbox) the generated handlers
//	      depend on are wired exactly as cmd/server does.
//
// Each test grabs a fresh Harness, applies all three migrations, and
// receives a ready-to-use Embed value. Cleanup runs via t.Cleanup so
// containers are torn down even on test failure.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// Harness is the assembled runtime tier — pool, cache, outbox, query
// cache, and the invalidation worker — against ephemeral Postgres +
// memcached containers. Tests interact with the typed handles directly,
// the same way generated server handlers do.
type Harness struct {
	Pool       runtime.Pool
	Cache      runtime.Cache
	Outbox     runtime.Outbox
	QueryCache *queryresult.Cache
	Reader     *read.Reader
	Worker     *invalidate.Worker

	pgxPool     *pgxpool.Pool
	pgContainer testcontainers.Container
	mcContainer testcontainers.Container

	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

// NewHarness spins up ephemeral Postgres + memcached containers, applies
// every migration in `migrations/` (in lexical order), and wires the
// runtime tier. The returned Harness is safe to use from a single goroutine
// (Outbox.Enqueue / Cache.Set are thread-safe; the test fixture itself
// isn't a shared resource).
//
// Cleanup is registered via t.Cleanup so partial failures still tear down
// containers — the test will leak a goroutine for at most workerStopTimeout
// (a few seconds) before t.Cleanup unblocks.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pgC, pgURL := startPostgres(t, ctx)
	mcC, mcAddr := startMemcached(t, ctx)

	// PG pool first so migrations run before the worker LISTENs on the
	// channel its outbox writes to.
	pool, err := pg.New(ctx, pg.Config{
		URL:                 pgURL,
		MaxConns:            8,
		MinConns:            2,
		MaxConnIdleTime:     5 * time.Minute,
		MaxConnLifetime:     time.Hour,
		HealthCheckPeriod:   30 * time.Second,
		QueryTimeoutDefault: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("pg.New: %v", err)
	}

	if err := applyMigrations(ctx, pool.Raw(), migrationsDir()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	mc, err := memcached.New(memcached.Config{
		Addrs:        []string{mcAddr},
		Timeout:      500 * time.Millisecond,
		MaxIdleConns: 4,
	})
	if err != nil {
		t.Fatalf("memcached.New: %v", err)
	}

	reader, err := read.New(mc, read.Config{
		LRUSize:       128,
		MaxValueBytes: 1 << 20,
		DefaultTTL:    10 * time.Minute,
		XFetchBeta:    1.0,
	})
	if err != nil {
		t.Fatalf("read.New: %v", err)
	}

	outbox := invalidate.NewOutbox()
	queryCache := queryresult.New(mc)
	worker, err := invalidate.NewWorker(pool.Raw(), mc, reader, queryCache, invalidate.WorkerConfig{
		Schema:        "atlantis",
		DrainInterval: 50 * time.Millisecond,
		BatchSize:     16,
		PointerTTL:    1 * time.Hour,
		AlertLag:      5 * time.Minute,
		BumpDebounce:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("invalidate.NewWorker: %v", err)
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_ = worker.Run(workerCtx)
	}()

	h := &Harness{
		Pool:         pool,
		Cache:        mc,
		Outbox:       outbox,
		QueryCache:   queryCache,
		Reader:       reader,
		Worker:       worker,
		pgxPool:      pool.Raw(),
		pgContainer:  pgC,
		mcContainer:  mcC,
		workerCancel: workerCancel,
		workerDone:   workerDone,
	}
	t.Cleanup(h.Close)
	return h
}

// PgxPool exposes the raw pgxpool so sub-package tests (e.g.,
// tests/integration/backfill) can construct workers that take the
// concrete pool type without reaching for an internal accessor.
func (h *Harness) PgxPool() *pgxpool.Pool { return h.pgxPool }

// WaitForInvalidations blocks until the outbox is empty, up to the timeout.
// Tests call this after a write to ensure the post-commit invalidation
// has actually landed in memcached before asserting a cache miss/hit.
func (h *Harness) WaitForInvalidations(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		err := h.pgxPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM atlantis.cache_invalidations`).Scan(&n)
		if err == nil && n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("outbox did not drain within %s", timeout)
}

// Close stops the worker, closes the pool, and terminates both containers.
// Idempotent; safe to call twice.
var harnessCloseOnce sync.Map

func (h *Harness) Close() {
	if _, loaded := harnessCloseOnce.LoadOrStore(h, struct{}{}); loaded {
		return
	}
	h.workerCancel()
	select {
	case <-h.workerDone:
	case <-time.After(5 * time.Second):
		// Worker is wedged; nothing left to do. Containers terminate below.
	}
	h.pgxPool.Close()
	bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if h.pgContainer != nil {
		_ = h.pgContainer.Terminate(bg)
	}
	if h.mcContainer != nil {
		_ = h.mcContainer.Terminate(bg)
	}
}

// ----- container bootstrap -----

func startPostgres(t *testing.T, ctx context.Context) (testcontainers.Container, string) {
	t.Helper()
	// timescaledb-ha includes pgvector + timescaledb out of the box, matching
	// docker-compose.yml. Using the same image avoids "works in compose, fails
	// in tests" surprises.
	c, err := tcpostgres.Run(ctx,
		"timescale/timescaledb-ha:pg16-all",
		tcpostgres.WithDatabase("atlantis"),
		tcpostgres.WithUsername("atlantis"),
		tcpostgres.WithPassword("atlantis"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	return c, url
}

func startMemcached(t *testing.T, ctx context.Context) (testcontainers.Container, string) {
	t.Helper()
	// Upstream memcached: wire-protocol compatible with Grafana's fork
	// and actually pullable from a public registry (the Grafana fork
	// isn't published standalone — see docker-compose.yml comment).
	req := testcontainers.ContainerRequest{
		Image:        "memcached:1.6.29-alpine",
		ExposedPorts: []string{"11211/tcp"},
		WaitingFor:   wait.ForLog("server listening").WithStartupTimeout(30 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start memcached: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("memcached host: %v", err)
	}
	port, err := c.MappedPort(ctx, "11211/tcp")
	if err != nil {
		t.Fatalf("memcached port: %v", err)
	}
	return c, fmt.Sprintf("%s:%s", host, port.Port())
}

// ----- migration runner -----

// applyMigrations applies every .up.sql under root, walking infra/ before
// tidectl/ so the hand-written infra schema (outbox + bookkeeping) lands
// before the codegen-emitted entity schema (whose trigger functions
// reference the infra-defined cache_invalidations table).
//
// We don't use golang-migrate here because we want the test process to
// control transaction boundaries precisely; for the integration harness,
// applying each migration as plain SQL is enough.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, root string) error {
	for _, sub := range []string{"infra", "tidectl"} {
		if err := applyMigrationsDir(ctx, pool, filepath.Join(root, sub)); err != nil {
			return err
		}
	}
	return nil
}

func applyMigrationsDir(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	var ups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".sql" {
			continue
		}
		if !endsWith(name, ".up.sql") {
			continue
		}
		ups = append(ups, name)
	}
	// Sort by filename — migrations are NNNN-prefixed so lex order = apply order.
	sortStrings(ups)

	for _, name := range ups {
		sql, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// endsWith / sortStrings are tiny helpers to keep the import list flat —
// they're called once each, importing strings and sort just for this would
// be noise.
func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// migrationsDir returns the absolute path to atlantis/migrations
// relative to this test file. Lets `go test` work from any directory.
func migrationsDir() string {
	// tests/integration is two levels under the repo root. Resolve via the
	// runtime working directory under which `go test` runs — that's the
	// package dir by default.
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "..", "..", "migrations")
}
