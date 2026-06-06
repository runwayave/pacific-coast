package sandbox

import (
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// LoadIR populates the sandbox catalog from a *dsl.IR. Each Entity in
// the IR becomes a TableDesc; non-table declarations (Ephemerals,
// Workflows, Jobs, custom Queries, custom Procedures) are skipped
// silently — sim doesn't model those, and the fidelity matrix already
// documents the routing decision.
//
// Per-entity failures are RESILIENT: an entity that can't translate
// (unrecognized type, missing PK, duplicate registration) is logged
// via the sandbox Warn callback and skipped — the rest of the IR
// still loads. A real caller schema commonly has 50+ entities and
// rejecting the whole boot because one entity has a quirky column
// type would be a foot-gun. Operators see the warnings on the
// sandbox page and can investigate per-entity.
//
// LoadIR is idempotent within a single call. Across calls a table
// re-registration is itself an error; LoadIR catches that too and
// continues.
//
// Hypertables collapse to regular tables — the sim doesn't
// time-partition — but TimeField is captured on the descriptor so the
// hypertable warn-once hook fires correctly on first query.
func (s *Sandbox) LoadIR(ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("sandbox: LoadIR called with nil IR")
	}
	warn := s.opts.Warn
	if warn == nil {
		warn = func(string) {}
	}
	cat := s.pool.Catalog()
	var loaded, skipped int
	for i := range ir.Entities {
		e := &ir.Entities[i]
		desc, err := buildTableDescFromEntity(e)
		if err != nil {
			warn(fmt.Sprintf("entity %s skipped: %v", e.ID(), err))
			skipped++
			continue
		}
		if err := cat.RegisterTable(desc); err != nil {
			warn(fmt.Sprintf("entity %s skipped (register): %v", e.ID(), err))
			skipped++
			continue
		}
		loaded++
	}
	// Non-fatal observability: tell the operator how many entities
	// the sim is actually backing. Surfaces silently-lost coverage.
	if skipped > 0 {
		warn(fmt.Sprintf("loaded %d entities; skipped %d (see prior warnings)", loaded, skipped))
	}
	return nil
}

// buildTableDescFromEntity translates one *dsl.Entity into a TableDesc.
// All IR semantics that sim honors at execution time are captured here;
// IR features that don't affect sim behavior (Cache block, Relations,
// Indexes for query planning) are deliberately ignored —
// secondary-index-driven scans are not yet wired.
func buildTableDescFromEntity(e *dsl.Entity) (*sim.TableDesc, error) {
	schema, name := schemaNameFor(e)

	cols := make([]sim.Column, 0, len(e.Fields))
	var identityCol string
	for i := range e.Fields {
		f := &e.Fields[i]
		kind, err := colKindFor(f.Type)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Name, err)
		}
		cols = append(cols, sim.Column{
			Name:     f.Name,
			Kind:     kind,
			Nullable: !f.NotNull,
		})
		if f.Identity || f.Serial {
			// Codegen treats both BIGSERIAL and GENERATED ALWAYS AS
			// IDENTITY as auto-increment; we capture the column name
			// once and let the sim's atomic counter feed it once
			// auto-id-on-insert is wired.
			identityCol = f.Name
		}
	}

	pkCols, err := primaryKeyColumns(e)
	if err != nil {
		return nil, err
	}

	return &sim.TableDesc{
		Schema:             schema,
		Name:               name,
		Cols:               cols,
		PKCols:             pkCols,
		SoftDeleteField:    e.SoftDeleteField,
		TouchOnUpdateField: e.TouchOnUpdateField,
		PartitionField:     e.PartitionField,
		TimeField:          e.TimeField,
		IdentityCol:        identityCol,
	}, nil
}

// schemaNameFor resolves the physical (schema, name) the simulator's
// catalog keys by. Default form mirrors codegen's
// `atlantis.<namespace>_<snake>`; the TableName override on the entity
// wins when set ("schema.table" or just "table"). Snake-casing on the
// default path follows the codegen convention exactly so a snapshot
// taken via the sim matches table names a real DB would carry.
func schemaNameFor(e *dsl.Entity) (string, string) {
	if e.TableName != "" {
		if i := strings.IndexByte(e.TableName, '.'); i >= 0 {
			return e.TableName[:i], e.TableName[i+1:]
		}
		// Bare table override stays in the atlantis schema.
		return "atlantis", e.TableName
	}
	return "atlantis", e.Namespace + "_" + toSnake(e.Name)
}

// toSnake converts CamelCase entity names to snake_case the way codegen
// emits them. Implementation is small enough to inline rather than
// pull a dependency.
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// primaryKeyColumns returns the entity's PK columns in declaration
// order. Composite-PK entities carry CompositePK explicitly; single-PK
// entities have it on the field via the `primary` modifier. Mutual
// exclusion is enforced at IR-lower time — we trust that contract and
// take the first applicable representation.
func primaryKeyColumns(e *dsl.Entity) ([]string, error) {
	if len(e.CompositePK) > 0 {
		return append([]string(nil), e.CompositePK...), nil
	}
	pf := e.PrimaryField()
	if pf == nil {
		return nil, fmt.Errorf("entity %s has no primary key", e.ID())
	}
	return []string{pf.Name}, nil
}

// colKindFor maps an IR FieldType to a sim ColKind.
//
// Arrays land on KindArray — the storage layer holds whatever Go slice
// the caller binds. Generated CRUD over array columns (INSERT / SELECT)
// works; PG-specific array operators (= ANY(col), @>, &&, unnest) stay
// unsupported and error at query time. The plan's "no arrays" stance
// was an over-promise — real caller schemas use text[] / int[] heavily
// and need at least round-trip support to boot.
//
// Unknown type names also degrade to KindBytes (opaque) rather than
// erroring, so a new PG type or a custom domain in a caller schema
// doesn't block sandbox boot. The trade-off: queries that compare
// against opaque columns fail in the executor with a clear error;
// schemas without such queries work fine.
func colKindFor(ft dsl.FieldType) (sim.ColKind, error) {
	if ft.Array {
		return sim.KindArray, nil
	}
	switch strings.ToLower(ft.Name) {
	case "smallint", "int", "integer", "bigint", "identity", "serial":
		return sim.KindInt64, nil
	case "text", "varchar", "citext", "uuid":
		return sim.KindString, nil
	case "boolean", "bool":
		return sim.KindBool, nil
	case "timestamptz", "timestamp", "date":
		return sim.KindTime, nil
	case "bytea", "jsonb":
		return sim.KindBytes, nil
	case "numeric":
		return sim.KindNumeric, nil
	case "vector":
		return sim.KindVector, nil
	}
	// Unknown PG type → opaque bytes. The sim won't be able to evaluate
	// type-specific operators on this column but the row round-trips
	// through INSERT/SELECT.
	return sim.KindBytes, nil
}
