//go:build integration

package backfill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// pgHarness is a minimal Postgres-only harness for the backfill tests.
// The broader tests/integration harness also spins up memcached + wires
// the cache tier; the backfill worker only needs pg, so we skip the
// extra container.
type pgHarness struct {
	Pool        *pgxpool.Pool
	pgContainer testcontainers.Container
	closeOnce   sync.Once
}

func newPGHarness(t *testing.T) *pgHarness {
	t.Helper()
	// 5-minute ceiling absorbs cold-start image pulls on CI runners.
	// postgres:16-alpine is ~80MB and starts in seconds locally; the
	// budget is for the first GitHub Actions runner of the day.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Vanilla postgres: the backfill tests don't need TimescaleDB or
	// pgvector, and using the slim image cuts cold-pull time from
	// ~90s+ (timescaledb-ha) to ~10s.
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("atlantis"),
		tcpostgres.WithUsername("atlantis"),
		tcpostgres.WithPassword("atlantis"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := applyMigrations(ctx, pool, migrationsDir()); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	h := &pgHarness{Pool: pool, pgContainer: c}
	t.Cleanup(h.Close)
	return h
}

func (h *pgHarness) Close() {
	h.closeOnce.Do(func() {
		if h.Pool != nil {
			h.Pool.Close()
		}
		if h.pgContainer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = h.pgContainer.Terminate(ctx)
		}
	})
}

// applyMigrations walks migrations/infra in lex order. The backfill
// tests don't need the tidectl-emitted entity migrations (no .atl
// fixtures), only the hand-written infra schema.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, root string) error {
	dir := filepath.Join(root, "infra")
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
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		ups = append(ups, name)
	}
	sort.Strings(ups)
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

func migrationsDir() string {
	cwd, _ := os.Getwd()
	// tests/integration/backfill → repo root is three levels up.
	return filepath.Join(cwd, "..", "..", "..", "migrations")
}
