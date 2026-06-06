package sandbox

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// Fixtures generates schema-valid rows for a sandbox by walking the
// IR-derived catalog and inserting per-column values through the same
// SQL path generated handlers use. The only input is the same .atl
// the user already writes — no prod data, no anonymization pipeline.
//
// Per-kind generators are driven by sandbox.Options.Seed so two
// sandboxes booted with the same seed produce identical row data.
// FK-graph auto-resolution is not implemented; callers seed parents
// before children.
type Fixtures struct{ sb *Sandbox }

// Fixtures returns the seed surface for this sandbox.
func (s *Sandbox) Fixtures() *Fixtures { return &Fixtures{sb: s} }

// BulkOptions tunes Bulk. Empty value yields sensible defaults.
type BulkOptions struct {
	// Seed overrides the sandbox's Options.Seed for this call. Useful
	// when seeding multiple tables independently from the same
	// sandbox — different seeds avoid PK collisions across tables that
	// share a counter-derived PK space.
	Seed int64

	// PKStart is the first integer-PK value generated. When 0 it
	// starts at 1 (matching PG BIGSERIAL semantics). For tables
	// without an integer PK this field is ignored.
	PKStart int64
}

// Bulk inserts n schema-valid rows into the table named by qualified
// (e.g. "atlantis.consumer_user"). Returns the number of rows
// successfully inserted, which is always n on success — a single
// failure aborts the whole batch and returns the underlying error.
//
// Generated values per ColKind:
//   - int64: monotonic counter starting at opts.PKStart (PK columns)
//     or seeded random in [0, 1e6) (non-PK columns).
//   - string: realistic word from a small lexicon, or email-shaped
//     when the column name suggests email.
//   - bool: 50/50 seeded random.
//   - timestamptz: epoch + (i × 1 minute) for monotone ordering.
//   - numeric: decimal-string in [0.00, 1000.00).
//   - bytes (JSONB): small JSON object literal.
//   - vector: unit vector of the column's declared dimension.
//
// Nullable columns get a NULL with 10% probability — enough to
// exercise NULL-handling code paths without making queries pathological.
//
// Bulk respects NotNull but does not evaluate CHECK constraints —
// rows that would fail a CHECK still insert successfully.
func (f *Fixtures) Bulk(ctx context.Context, qualified string, n int, opts BulkOptions) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	desc := f.sb.Catalog().Lookup(qualified)
	if desc == nil {
		return 0, fmt.Errorf("%w: %s", ErrUnknownEntity, qualified)
	}

	seed := opts.Seed
	if seed == 0 {
		seed = f.sb.opts.Seed
	}
	rng := rand.New(rand.NewSource(seed))
	pkCounter := opts.PKStart
	if pkCounter == 0 {
		pkCounter = 1
	}

	// Build the INSERT statement once — the value list is built per
	// row from placeholders matching column order.
	insertSQL := buildBulkInsertSQL(desc)

	for k := 0; k < n; k++ {
		args := make([]any, len(desc.Cols))
		for i, c := range desc.Cols {
			// 10% NULL on nullable columns — enough to exercise NULL
			// paths without dominating the dataset.
			if c.Nullable && rng.Intn(10) == 0 {
				args[i] = nil
				continue
			}
			args[i] = generateValue(c, rng, &pkCounter, isPKColumn(desc, c.Name), k)
		}
		if _, err := f.sb.pool.Exec(ctx, insertSQL, args...); err != nil {
			return k, fmt.Errorf("sandbox fixtures: row %d: %w", k, err)
		}
	}
	return n, nil
}

// buildBulkInsertSQL emits `INSERT INTO "schema"."name" ("c1","c2") VALUES ($1, $2)`.
// We always name every column — defaults aren't relied on; fixtures
// pre-fills every value explicitly so the row passes regardless of
// whether codegen's COALESCE-default shape is in play.
func buildBulkInsertSQL(desc *sim.TableDesc) string {
	var b strings.Builder
	b.WriteString(`INSERT INTO "`)
	b.WriteString(desc.Schema)
	b.WriteString(`"."`)
	b.WriteString(desc.Name)
	b.WriteString(`" (`)
	for i, c := range desc.Cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(c.Name)
		b.WriteByte('"')
	}
	b.WriteString(") VALUES (")
	for i := range desc.Cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("$")
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(")")
	return b.String()
}

// generateValue produces one realistic value for one column. Driven
// by ColKind + column name (so "email" generates a valid email shape,
// "name" generates a name-shaped string, etc.).
func generateValue(c sim.Column, rng *rand.Rand, pkCounter *int64, isPK bool, rowIdx int) any {
	switch c.Kind {
	case sim.KindInt64:
		if isPK {
			v := *pkCounter
			*pkCounter++
			return v
		}
		return rng.Int63n(1_000_000)
	case sim.KindString:
		return generateString(c.Name, rng, rowIdx)
	case sim.KindBool:
		return rng.Intn(2) == 0
	case sim.KindTime:
		// Monotonic by row — matches the "events ordered in time"
		// shape most timeline data takes.
		return time.Unix(0, 0).UTC().Add(time.Duration(rowIdx) * time.Minute)
	case sim.KindNumeric:
		// Numeric is stored as string in the sim per project memory;
		// generate decimal-formatted strings to match.
		v := rng.Float64() * 1000
		return fmt.Sprintf("%.2f", v)
	case sim.KindBytes:
		// JSONB-shaped opaque bytes — small object so existing
		// JSONB-naive code paths don't choke on huge blobs.
		return []byte(fmt.Sprintf(`{"v": %d}`, rng.Intn(1000)))
	case sim.KindVector:
		// Unit vector hardcoded to 8 dimensions — ColKind doesn't carry
		// the declared dim, and the IR-thread of FieldType.VecDim is
		// not wired here yet. Vector columns whose schema declares a
		// dimension other than 8 will round-trip through INSERT but
		// fail pgvector distance ops at query time.
		return generateUnitVector(rng, 8)
	}
	return nil
}

// generateString returns a name- or column-aware string. The lexicon
// is small enough to be obvious in test output (we don't want fixtures
// to look like real prod data — that's outside scope).
func generateString(colName string, rng *rand.Rand, rowIdx int) string {
	lc := strings.ToLower(colName)
	switch {
	case strings.Contains(lc, "email"):
		return fmt.Sprintf("user%d@example.com", rowIdx+1)
	case strings.Contains(lc, "url"):
		return fmt.Sprintf("https://example.com/p/%d", rowIdx+1)
	case strings.Contains(lc, "uuid"), strings.Contains(lc, "id"):
		return fmt.Sprintf("uuid-%08d", rowIdx+1)
	case strings.Contains(lc, "name"), strings.Contains(lc, "title"):
		// Two-word readable name from a tiny pool.
		first := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot"}
		second := []string{"Quark", "Bismuth", "Quartz", "Onyx", "Cobalt", "Indigo"}
		return first[rng.Intn(len(first))] + " " + second[rng.Intn(len(second))]
	case strings.Contains(lc, "plan"), strings.Contains(lc, "status"):
		pool := []string{"pending", "active", "trial", "pro"}
		return pool[rng.Intn(len(pool))]
	}
	// Default: alphanumeric token.
	return fmt.Sprintf("v%06d", rng.Intn(1_000_000))
}

// generateUnitVector returns a random unit vector of dimension dim.
// Useful when seeding pgvector columns; matches what real embedding
// pipelines produce (post-L2-normalization).
func generateUnitVector(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var sumsq float64
	for i := range v {
		// Standard normal via Box-Muller — closest to typical
		// embedding distributions without importing distuv.
		v[i] = float32(rng.NormFloat64())
		sumsq += float64(v[i]) * float64(v[i])
	}
	norm := math.Sqrt(sumsq)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

// isPKColumn checks if a column is part of the PK. Defined here as a
// small helper so generateValue's interface stays clean.
func isPKColumn(desc *sim.TableDesc, name string) bool {
	for _, pk := range desc.PKCols {
		if pk == name {
			return true
		}
	}
	return false
}
