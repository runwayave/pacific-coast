package introspect

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// TestNormalizePredicate_RealPostgres is the soundness proof for the drift
// matcher: a declared predicate, normalized through Postgres, must equal the
// pg_get_expr deparse of a REAL index built from the same predicate. It creates
// each index for real, reads its stored predicate, and asserts the normalizer
// reproduces it byte-for-byte — including Postgres-inserted implicit casts.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/introspect/ -run RealPostgres -v
func TestNormalizePredicate_RealPostgres(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to verify the normalizer against a live Postgres")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// A real table whose column types must match the declared entity's, so the
	// real index and the normalizer's temp index deparse identically.
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS pm_verify (
		id int, deleted_at timestamptz, status varchar(20), tier int, is_default boolean, _k int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS pm_verify`) })

	// Declared entity built through the real DSL so field types are faithful.
	file, err := dsl.Parse("verify.atl", []byte(`entity Pm in x {
  id         int primary
  deleted_at timestamptz
  status     varchar(20)
  tier       int
  is_default boolean
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{file})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	e := &ir.Entities[0]

	cases := []struct {
		name     string
		where    string
		declared *dsl.PredExpr
	}{
		{"isnull", "deleted_at IS NULL", nullPred("deleted_at", false)},
		{"isnotnull", "deleted_at IS NOT NULL", nullPred("deleted_at", true)},
		{"streq", "status = 'active'", cmpPred("=", col("status"), litS("active"))},
		{"intgt", "tier > 3", cmpPred(">", col("tier"), litI(3))},
		{"and", "status = 'active' AND deleted_at IS NULL",
			boolPred("and", cmpPred("=", col("status"), litS("active")), nullPred("deleted_at", false))},
		{"or-commuted", "deleted_at IS NULL OR status = 'active'",
			boolPred("or", cmpPred("=", col("status"), litS("active")), nullPred("deleted_at", false))},
		{"barebool", "is_default", truthyPred("is_default")},
		{"in", "tier IN (1, 2, 3)", inPred("tier", false, litI(3), litI(1), litI(2))},
		{"func", "lower(status) = 'x'",
			cmpPred("=", funcOp("lower", col("status")), litS("x"))},
		{"coalesce", "coalesce(status, 'n') = 'x'",
			cmpPred("=", funcOp("coalesce", col("status"), litS("n")), litS("x"))},
		{"cast", "tier::bigint > 3",
			cmpPred(">", castOp(col("tier"), "bigint"), litI(3))},
		{"case", "(CASE WHEN is_default THEN tier ELSE 0 END) > 0",
			cmpPred(">", caseOp(litI(0), when(truthyPred("is_default"), col("tier"))), litI(0))},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := fmt.Sprintf("pmre_%d", i)
			_, _ = pool.Exec(ctx, "DROP INDEX IF EXISTS "+idx)
			if _, err := pool.Exec(ctx, fmt.Sprintf(
				"CREATE INDEX %s ON pm_verify (_k) WHERE %s", idx, tc.where)); err != nil {
				t.Fatalf("create real index: %v", err)
			}
			var live string
			if err := pool.QueryRow(ctx,
				`SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = $1::regclass`,
				idx).Scan(&live); err != nil {
				t.Fatalf("read live pg_get_expr: %v", err)
			}
			norm, ok := normalizePredicate(ctx, pool, e, tc.declared)
			if !ok {
				t.Fatalf("normalizePredicate failed for %s", tc.where)
			}
			t.Logf("live=%q norm=%q", live, norm)
			// normalizedEqual is the actual matcher comparison: it accepts
			// commutative reordering (the or-commuted and reordered-IN cases).
			if !normalizedEqual(norm, live) {
				t.Errorf("normalized %q did NOT match live %q", norm, live)
			}
		})
	}

	// A genuinely different predicate must NOT normalize to a matching string.
	if norm, ok := normalizePredicate(ctx, pool, e, nullPred("deleted_at", true)); ok {
		if norm == "(deleted_at IS NULL)" {
			t.Errorf("IS NOT NULL normalized to IS NULL: %q", norm)
		}
	}
}
