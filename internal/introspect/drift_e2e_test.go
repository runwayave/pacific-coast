package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestDetectUniqueIndexDrift_EndToEnd exercises the real public entry point
// against a live Postgres: a real bare partial-UNIQUE index with a rich
// predicate, a declared `.atl` that should recognize it (no drift) or not
// (drift), through DetectUniqueIndexDrift → buildDeclaredUniques → classify →
// the Postgres normalizer → pgcompare. Also runs the apply-path variant where
// the detector is handed a pgx.Tx (savepoint) rather than the pool.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/introspect/ -run EndToEnd -v
func TestDetectUniqueIndexDrift_EndToEnd(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the end-to-end drift test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// A real table with a bare partial-UNIQUE index carrying a rich predicate
	// (boolean structure over a varchar + a timestamp — the implicit-cast case).
	mustExec(t, ctx, pool, `DROP TABLE IF EXISTS public.et`)
	mustExec(t, ctx, pool, `CREATE TABLE public.et (
		id int PRIMARY KEY, sku text, status varchar(20), deleted_at timestamptz)`)
	mustExec(t, ctx, pool, `CREATE UNIQUE INDEX et_sku_live ON public.et (sku)
		WHERE status = 'active' AND deleted_at IS NULL`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.et`) })

	lower := func(atl string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("e2e.atl", []byte(atl))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}

	// Declares the same predicate (conjuncts written in the OTHER order, to also
	// prove commutative recognition through the whole stack).
	matchIR := lower(`entity Et in x {
  table "public.et"
  id         int primary
  sku        text
  status     varchar(20)
  deleted_at timestamptz
  unique index partial by sku where deleted_at is null and status = "active"
}`)
	// Declares a genuinely different predicate.
	mismatchIR := lower(`entity Et in x {
  table "public.et"
  id         int primary
  sku        text
  status     varchar(20)
  deleted_at timestamptz
  unique index partial by sku where deleted_at is not null and status = "active"
}`)

	t.Run("recognized via pool", func(t *testing.T) {
		drift, _, err := DetectUniqueIndexDrift(ctx, pool, matchIR)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if len(drift) != 0 {
			t.Errorf("expected NO drift (declared predicate recognized), got %+v", drift)
		}
	})

	t.Run("mismatch is drift", func(t *testing.T) {
		drift, _, err := DetectUniqueIndexDrift(ctx, pool, mismatchIR)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		if len(drift) != 1 {
			t.Fatalf("expected 1 drift, got %d: %+v", len(drift), drift)
		}
		if drift[0].IndexName != "et_sku_live" {
			t.Errorf("drift index = %q, want et_sku_live", drift[0].IndexName)
		}
	})

	// The apply path hands the detector a pgx.Tx; the normalizer must work via a
	// savepoint nested in it and leave the outer tx usable.
	t.Run("recognized via apply tx (savepoint)", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		drift, _, err := DetectUniqueIndexDrift(ctx, tx, matchIR)
		if err != nil {
			t.Fatalf("detect in tx: %v", err)
		}
		if len(drift) != 0 {
			t.Errorf("expected NO drift in tx path, got %+v", drift)
		}
		// outer tx still usable after the normalizer's savepoints
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Errorf("outer tx unusable after normalize: %v", err)
		}
	})
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
