package introspect

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/predsql"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// DBTX is the capability the unique-index drift detector needs: read queries
// plus the ability to open a roll-backable sub-transaction for predicate
// normalization. Both *pgxpool.Pool and pgx.Tx satisfy it — the pool acquires a
// connection and begins a transaction; a tx opens a savepoint.
type DBTX interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// normalizePredicate returns the pg_get_expr deparse of a declared predicate as
// Postgres itself would store it — the SAME normalized form the live side is
// read in. Rather than reimplement Postgres's expression canonicalization
// (implicit casts, operator resolution, IN→ANY, commutativity, …) in Go and risk
// a false-accept, it renders the predicate onto a throwaway TEMP table whose
// columns carry the entity's real types, reads the stored index predicate back,
// and rolls everything back. Comparing two such strings is sound by
// construction: equivalence is decided by the same engine that enforces the
// index. Creating the index also validates the predicate is a legal (immutable)
// index predicate — a volatile one fails here exactly as it would on the real
// table.
//
// ok=false means the predicate could not be normalized (unknown column, illegal
// index predicate, or a DB error) — the caller treats that as "no match", so the
// live index stays drift. Everything happens inside a sub-transaction that is
// always rolled back: no persistent objects, no lock on any real table.
func normalizePredicate(ctx context.Context, db DBTX, e *dsl.Entity, pred *dsl.PredExpr) (string, bool) {
	cols := dedupeStrings(pred.Columns())
	if len(cols) == 0 {
		return "", false
	}
	defs := make([]string, 0, len(cols)+1)
	for _, name := range cols {
		f := e.FindField(name)
		if f == nil {
			return "", false
		}
		defs = append(defs, schema.QuoteIdent(name)+" "+schema.SQLType(f.Type))
	}
	// A guaranteed btree-indexable key column so the index build never fails on
	// a non-indexable predicate-column type (jsonb, etc.).
	defs = append(defs, "_k integer")

	tx, err := db.Begin(ctx)
	if err != nil {
		return "", false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "CREATE TEMP TABLE _atl_prednorm ("+strings.Join(defs, ", ")+")"); err != nil {
		return "", false
	}
	if _, err := tx.Exec(ctx, "CREATE INDEX _atl_predidx ON _atl_prednorm (_k) WHERE "+predsql.Render(pred)); err != nil {
		return "", false
	}
	var got string
	if err := tx.QueryRow(ctx,
		`SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = '_atl_predidx'::regclass`).Scan(&got); err != nil {
		return "", false
	}
	return got, true
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
