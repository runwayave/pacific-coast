package introspect

// drift.go detects one specific, high-confidence kind of schema drift that
// the v1 adopt/diff path is structurally blind to (see the package comment
// in postgres.go): a *bare* UNIQUE index — one created by `CREATE UNIQUE
// INDEX`, with no backing UNIQUE constraint — on columns the declared schema
// manages but does NOT declare unique.
//
// Why only this shape:
//   - Uniqueness atlantis itself declares (`unique`, `unique by ...`) is
//     emitted as a UNIQUE *constraint*, which Postgres backs with an
//     auto-created unique index whose `pg_index` row is owned by a
//     `pg_constraint` row (conindid). We exclude those structurally so the
//     detector can never fire on atlantis's own output.
//   - A legacy `CREATE UNIQUE INDEX` (e.g. a pre-adopt migration) has no
//     constraint row, so it survives the filter. This is exactly the class
//     that silently rejects legitimate writes — a uniqueness the schema
//     author never asked for and cannot see.
//   - The DSL cannot express a unique secondary index at all, so any bare
//     unique index on declared columns is, by construction, undeclared.
//
// The detector is read-only and reports; it never drops anything. The
// apply path decides policy (refuse vs. ATLANTIS_ALLOW_INDEX_DRIFT override).

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// UniqueIndexDrift is one live bare-unique index whose enforced uniqueness
// the declared schema does not account for. JSON-tagged so it round-trips
// through the admin PlanResponse to the CLI alongside the extension status.
type UniqueIndexDrift struct {
	EntityID  string   `json:"entity_id"`
	Schema    string   `json:"schema"`
	Table     string   `json:"table"`
	IndexName string   `json:"index_name"`
	Columns   []string `json:"columns"`
	// Partial is true for a `... WHERE <pred>` index. A partial unique is
	// semantically weaker than a full unique (it permits duplicates outside
	// the predicate) and the DSL cannot express it at all, so a partial
	// unique on declared columns is always drift regardless of any declared
	// full-uniqueness on the same columns.
	Partial   bool   `json:"partial,omitempty"`
	Predicate string `json:"predicate,omitempty"`
}

// DropStatement is the exact DDL an operator runs to resolve the drift. It
// uses the *live* index name verbatim (never a reconstructed one, which can
// differ under Postgres's 63-char identifier truncation).
func (d UniqueIndexDrift) DropStatement() string {
	return fmt.Sprintf("DROP INDEX %s.%s;", quoteIdentDDL(d.Schema), quoteIdentDDL(d.IndexName))
}

// Describe renders the columns (and partial predicate) for an operator-
// facing message.
func (d UniqueIndexDrift) Describe() string {
	cols := "(" + strings.Join(d.Columns, ", ") + ")"
	if d.Partial {
		return cols + " WHERE " + d.Predicate
	}
	return cols
}

// DetectUniqueIndexDrift introspects the live DB for bare unique indexes on
// the declared tables and returns the ones the schema doesn't account for,
// plus advisory notes (e.g. expression indexes that couldn't be analyzed).
// Read-only; safe to run against s.pool at plan time or inside the apply tx.
func DetectUniqueIndexDrift(ctx context.Context, q Querier, declaredIR *dsl.IR) ([]UniqueIndexDrift, []string, error) {
	if declaredIR == nil {
		return nil, nil, fmt.Errorf("introspect: declaredIR is required")
	}
	declared := buildDeclaredUniques(declaredIR)
	if len(declared) == 0 {
		return nil, nil, nil
	}
	pairs := make([]physRef, 0, len(declared))
	for r := range declared {
		pairs = append(pairs, r)
	}
	live, skippedExpr, err := loadBareUniqueIndexes(ctx, q, pairs)
	if err != nil {
		return nil, nil, err
	}
	drift := classifyUniqueIndexDrift(declared, live)

	var notes []string
	if skippedExpr > 0 {
		notes = append(notes, fmt.Sprintf("%d unique expression index(es) on declared tables were not analyzed (their columns are expressions, not plain columns) — audit them out-of-band", skippedExpr))
	}
	return drift, notes, nil
}

// declaredUnique captures, per physical table, the columns the schema
// manages and the column-sets it declares unique. uniqueSets is keyed by
// the order-independent uniqueKey so matching is set-based (UNIQUE(a,b) ≡
// UNIQUE(b,a)).
type declaredUnique struct {
	entityID   string
	fields     map[string]bool
	uniqueSets map[string]bool
}

func buildDeclaredUniques(ir *dsl.IR) map[physRef]declaredUnique {
	out := make(map[physRef]declaredUnique, len(ir.Entities))
	for i := range ir.Entities {
		e := &ir.Entities[i]
		schema, table := physical(e)
		du := declaredUnique{
			entityID:   e.ID(),
			fields:     make(map[string]bool, len(e.Fields)),
			uniqueSets: make(map[string]bool),
		}
		for _, f := range e.Fields {
			du.fields[f.Name] = true
			if f.Unique || f.Primary {
				du.uniqueSets[uniqueKey([]string{f.Name})] = true
			}
		}
		for _, u := range e.Uniques {
			du.uniqueSets[uniqueKey(u.Fields)] = true
		}
		if len(e.CompositePK) > 0 {
			du.uniqueSets[uniqueKey(e.CompositePK)] = true
		}
		out[physRef{schema, table}] = du
	}
	return out
}

// liveUniqueIndex is one row from loadBareUniqueIndexes.
type liveUniqueIndex struct {
	schema, table, name string
	columns             []string
	partial             bool
	predicate           string
}

// classifyUniqueIndexDrift is the pure decision: a live bare-unique index is
// drift when every column it covers is a declared field AND it is not
// already declared unique (partial uniques are always drift — see the
// Partial field doc). An index touching any undeclared column is the
// operator's private business and is left alone.
func classifyUniqueIndexDrift(declared map[physRef]declaredUnique, live map[physRef][]liveUniqueIndex) []UniqueIndexDrift {
	var out []UniqueIndexDrift
	for ref, idxs := range live {
		du, ok := declared[ref]
		if !ok {
			continue // table not declared → not ours to judge
		}
		for _, idx := range idxs {
			if len(idx.columns) == 0 {
				continue
			}
			allDeclared := true
			for _, c := range idx.columns {
				if !du.fields[c] {
					allDeclared = false
					break
				}
			}
			if !allDeclared {
				continue
			}
			// A full unique whose columns match a declared uniqueness is
			// exactly what the schema asked for. A partial unique can never
			// be what the schema asked for (the DSL can't express it).
			if !idx.partial && du.uniqueSets[uniqueKey(idx.columns)] {
				continue
			}
			out = append(out, UniqueIndexDrift{
				EntityID:  du.entityID,
				Schema:    idx.schema,
				Table:     idx.table,
				IndexName: idx.name,
				Columns:   idx.columns,
				Partial:   idx.partial,
				Predicate: idx.predicate,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].IndexName < out[j].IndexName
	})
	return out
}

// loadBareUniqueIndexes reads valid, non-primary unique indexes that are NOT
// backed by a constraint, for the given tables. Returns them keyed by table
// plus a count of unique indexes skipped because they index an expression
// (attnum 0) rather than plain columns.
func loadBareUniqueIndexes(ctx context.Context, q Querier, pairs []physRef) (map[physRef][]liveUniqueIndex, int, error) {
	schemas, tables := splitPairs(pairs)
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT
    n.nspname,
    c.relname,
    ic.relname AS index_name,
    (i.indpred IS NOT NULL) AS is_partial,
    CASE WHEN i.indpred IS NOT NULL
         THEN pg_get_expr(i.indpred, i.indrelid)
         ELSE '' END AS predicate,
    (SELECT array_agg(a.attname ORDER BY x.ord)
       FROM unnest(string_to_array(i.indkey::text, ' ')::int[]) WITH ORDINALITY AS x(attnum, ord)
       JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = x.attnum
      WHERE x.attnum <> 0) AS col_names,
    EXISTS (
        SELECT 1
          FROM unnest(string_to_array(i.indkey::text, ' ')::int[]) AS y(attnum)
         WHERE y.attnum = 0
    ) AS has_expr
FROM pg_index i
JOIN pg_class c       ON c.oid = i.indrelid
JOIN pg_class ic      ON ic.oid = i.indexrelid
JOIN pg_namespace n   ON n.oid = c.relnamespace
JOIN targets tg       ON tg.schema = n.nspname AND tg.table_name = c.relname
WHERE i.indisunique  = true
  AND i.indisprimary = false
  AND i.indisvalid   = true
  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid)`,
		schemas, tables)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make(map[physRef][]liveUniqueIndex)
	skipped := 0
	for rows.Next() {
		var (
			s, t, name, predicate string
			isPartial, hasExpr    bool
			cols                  []string
		)
		if err := rows.Scan(&s, &t, &name, &isPartial, &predicate, &cols, &hasExpr); err != nil {
			return nil, 0, err
		}
		if hasExpr || len(cols) == 0 {
			skipped++
			continue
		}
		key := physRef{s, t}
		out[key] = append(out[key], liveUniqueIndex{
			schema:    s,
			table:     t,
			name:      name,
			columns:   cols,
			partial:   isPartial,
			predicate: predicate,
		})
	}
	return out, skipped, rows.Err()
}

// uniqueKey is the order-independent identity of a column set, so a live
// index on (b, a) matches a declared `unique by a, b`.
func uniqueKey(cols []string) string {
	cp := append([]string(nil), cols...)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

// quoteIdentDDL double-quotes a Postgres identifier for the remediation
// statement. Identifiers come from pg_catalog, but quoting keeps the
// suggested DDL copy-paste-safe for mixed-case or reserved names.
func quoteIdentDDL(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
