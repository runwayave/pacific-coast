// Package codegen takes a dsl.IR and emits the artifacts callers consume:
// SQL migrations, .proto files, gRPC server stubs, typed clients, and cache
// key derivation. It also diffs two IRs to classify schema changes.
package codegen

import (
	"fmt"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ChangeClass selects how a change must be applied.
type ChangeClass int

const (
	// ClassAdditive is auto-applied by `tide apply`. Examples: add entity,
	// add nullable field, add index, loosen NOT NULL, widen a type, add
	// default, add cache hint.
	ClassAdditive ChangeClass = iota

	// ClassBackfillRequired needs explicit backfill SQL before applying.
	// Examples: NOT NULL on an existing column, type narrowing, FK on a
	// populated table.
	ClassBackfillRequired

	// ClassCrossCallerBreaking opens a PR in atlantis. Examples: drop a
	// field other callers may read, remove an entity, rename a field/entity.
	// The diff engine does not know which callers are pinned — the Admin
	// service composes that knowledge with this classification to decide
	// whether to auto-apply or escalate.
	ClassCrossCallerBreaking
)

// String returns the human-readable label for the class.
func (c ChangeClass) String() string {
	switch c {
	case ClassAdditive:
		return "additive"
	case ClassBackfillRequired:
		return "backfill-required"
	case ClassCrossCallerBreaking:
		return "cross-caller-breaking"
	}
	return "unknown"
}

// ChangeKind identifies the structural shape of one diff entry. Together with
// ChangeClass these drive the SQL emitter.
type ChangeKind string

const (
	KindEntityAdded   ChangeKind = "entity_added"
	KindEntityRemoved ChangeKind = "entity_removed"

	KindFieldAdded   ChangeKind = "field_added"
	KindFieldRemoved ChangeKind = "field_removed"

	// KindFieldNotNullTightened: NOT NULL added to existing nullable column.
	KindFieldNotNullTightened ChangeKind = "field_not_null_tightened"
	// KindFieldNotNullLoosened: NOT NULL removed.
	KindFieldNotNullLoosened ChangeKind = "field_not_null_loosened"

	KindFieldTypeChanged       ChangeKind = "field_type_changed"
	KindFieldDefaultChanged    ChangeKind = "field_default_changed"
	KindFieldUniqueAdded       ChangeKind = "field_unique_added"
	KindFieldUniqueRemoved     ChangeKind = "field_unique_removed"
	KindFieldReferenceAdded    ChangeKind = "field_reference_added"
	KindFieldReferenceRemoved  ChangeKind = "field_reference_removed"
	KindFieldReferenceModified ChangeKind = "field_reference_modified"

	KindIndexAdded   ChangeKind = "index_added"
	KindIndexRemoved ChangeKind = "index_removed"

	// KindFieldSerialAdded: BIGSERIAL added to an existing column.
	// Sequence must be seeded to MAX(col)+1 before apply or new inserts
	// collide with existing rows.
	KindFieldSerialAdded ChangeKind = "field_serial_added"
	// KindFieldSerialRemoved: BIGSERIAL removed. Callers that relied on
	// auto-increment must now supply the column explicitly.
	KindFieldSerialRemoved ChangeKind = "field_serial_removed"

	// KindFieldBackfill* track changes to the `backfill "<expr>"` field
	// modifier. The modifier itself causes no schema change — it's the
	// signal `tide apply --backfill` uses to know how to populate the
	// column when an associated NOT NULL or new-NOT-NULL change needs
	// existing-row data. Classified Additive on its own; the apply-time
	// rejection comes from the paired NotNull change.
	KindFieldBackfillAdded   ChangeKind = "field_backfill_added"
	KindFieldBackfillRemoved ChangeKind = "field_backfill_removed"
	KindFieldBackfillChanged ChangeKind = "field_backfill_changed"

	KindCacheChanged ChangeKind = "cache_changed"

	KindQueryTimeoutChanged ChangeKind = "query_timeout_changed"
)

// Change is one structural difference between two IRs.
type Change struct {
	Kind     ChangeKind  `json:"kind"`
	Class    ChangeClass `json:"class"`
	EntityID string      `json:"entity_id"`        // namespace.Name
	Field    string      `json:"field,omitempty"`  // for field/index changes
	Detail   string      `json:"detail,omitempty"` // human-readable summary
	From     any         `json:"from,omitempty"`
	To       any         `json:"to,omitempty"`
}

// Diff is the full set of changes between two IRs, partitioned by class.
type Diff struct {
	Additive         []Change `json:"additive,omitempty"`
	BackfillRequired []Change `json:"backfill_required,omitempty"`
	Breaking         []Change `json:"breaking,omitempty"`
}

// IsEmpty reports whether the diff has no changes.
func (d *Diff) IsEmpty() bool {
	return len(d.Additive)+len(d.BackfillRequired)+len(d.Breaking) == 0
}

// HighestClass returns the most-restrictive class present, which drives
// whether `tide apply` can auto-apply, requires backfill, or escalates to a PR.
func (d *Diff) HighestClass() ChangeClass {
	if len(d.Breaking) > 0 {
		return ClassCrossCallerBreaking
	}
	if len(d.BackfillRequired) > 0 {
		return ClassBackfillRequired
	}
	return ClassAdditive
}

// ComputeDiff diffs old → new. Either IR may be nil; nil means "no schema yet".
//
// Determinism: the returned change order is stable (entities sorted by ID,
// fields by name within each entity, then index/cache changes), which is
// important because the SQL emitter and tests depend on it.
func ComputeDiff(oldIR, newIR *dsl.IR) *Diff {
	d := &Diff{}
	oldByID := indexByID(oldIR)
	newByID := indexByID(newIR)

	// Stable union of entity IDs.
	ids := make([]string, 0, len(oldByID)+len(newByID))
	seen := map[string]bool{}
	for id := range oldByID {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	for id := range newByID {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		oldE, hasOld := oldByID[id]
		newE, hasNew := newByID[id]
		switch {
		case hasOld && !hasNew:
			// Entity removed — breaking. Even if no caller reads it, dropping it
			// can break the in-flight queries from a caller pinned to an older
			// BSR version. The Admin service may downgrade based on consumer
			// impact, but the diff engine's job is to flag the worst case.
			d.Breaking = append(d.Breaking, Change{
				Kind:     KindEntityRemoved,
				Class:    ClassCrossCallerBreaking,
				EntityID: id,
				Detail:   "entity removed",
				From:     oldE,
			})
		case !hasOld && hasNew:
			d.Additive = append(d.Additive, Change{
				Kind:     KindEntityAdded,
				Class:    ClassAdditive,
				EntityID: id,
				Detail:   "entity added",
				To:       newE,
			})
		default:
			diffEntity(oldE, newE, d)
		}
	}
	return d
}

// ---- per-entity diffs ----

func diffEntity(oldE, newE *dsl.Entity, d *Diff) {
	diffFields(oldE, newE, d)
	diffIndexes(oldE, newE, d)
	diffCache(oldE, newE, d)
	diffQueryTimeout(oldE, newE, d)
}

func diffFields(oldE, newE *dsl.Entity, d *Diff) {
	oldFields := fieldsByName(oldE)
	newFields := fieldsByName(newE)

	names := mergedNames(oldFields, newFields)
	for _, name := range names {
		of, hasOld := oldFields[name]
		nf, hasNew := newFields[name]
		switch {
		case hasOld && !hasNew:
			d.Breaking = append(d.Breaking, Change{
				Kind:     KindFieldRemoved,
				Class:    ClassCrossCallerBreaking,
				EntityID: newE.ID(),
				Field:    name,
				Detail:   "field removed",
				From:     of,
			})
		case !hasOld && hasNew:
			// New field. Backfill-required iff NOT NULL with no DEFAULT and
			// the entity already existed (i.e., there may be existing rows).
			class := ClassAdditive
			detail := "field added"
			if nf.NotNull && nf.Default == nil {
				class = ClassBackfillRequired
				detail = "field added NOT NULL with no DEFAULT — requires backfill"
			}
			ch := Change{
				Kind:     KindFieldAdded,
				Class:    class,
				EntityID: newE.ID(),
				Field:    name,
				Detail:   detail,
				To:       nf,
			}
			d.append(ch)
			// Detect FK-on-populated-table separately. Adding a reference
			// after the column existed isn't covered here (the column itself
			// is new) — but we still emit the additive-reference change so
			// the SQL emitter can generate the FK constraint.
			if nf.Ref != nil {
				d.Additive = append(d.Additive, Change{
					Kind:     KindFieldReferenceAdded,
					Class:    ClassAdditive,
					EntityID: newE.ID(),
					Field:    name,
					Detail:   "reference added on newly-introduced field",
					To:       nf.Ref,
				})
			}
		default:
			diffField(newE.ID(), of, nf, d)
		}
	}
}

func diffField(entityID string, oldF, newF *dsl.Field, d *Diff) {
	// Type change. We classify type narrowing as backfill-required, widening
	// as additive. We treat any non-equal type as breaking unless we can
	// recognize it as a safe widening.
	if !typeEqual(oldF.Type, newF.Type) {
		class := classifyTypeChange(oldF.Type, newF.Type)
		d.append(Change{
			Kind:     KindFieldTypeChanged,
			Class:    class,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   fmt.Sprintf("type %s → %s", typeString(oldF.Type), typeString(newF.Type)),
			From:     oldF.Type,
			To:       newF.Type,
		})
	}

	// NOT NULL changes.
	switch {
	case !oldF.NotNull && newF.NotNull:
		// Tightening NOT NULL on an existing column requires backfill iff
		// there's no DEFAULT in the new schema (DEFAULT covers existing rows).
		class := ClassBackfillRequired
		detail := "NOT NULL tightened — requires backfill (or a DEFAULT)"
		if newF.Default != nil {
			class = ClassAdditive
			detail = "NOT NULL tightened with DEFAULT — safe"
		}
		d.append(Change{
			Kind:     KindFieldNotNullTightened,
			Class:    class,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   detail,
		})
	case oldF.NotNull && !newF.NotNull:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldNotNullLoosened,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "NOT NULL loosened",
		})
	}

	// UNIQUE changes.
	switch {
	case !oldF.Unique && newF.Unique:
		// Adding UNIQUE to an existing column may fail if data has duplicates.
		// We classify as backfill-required so a human verifies / dedupes.
		d.append(Change{
			Kind:     KindFieldUniqueAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "UNIQUE added — verify no duplicates exist",
		})
	case oldF.Unique && !newF.Unique:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldUniqueRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "UNIQUE removed",
		})
	}

	// DEFAULT changes.
	if !defaultsEqual(oldF.Default, newF.Default) {
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldDefaultChanged,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "DEFAULT changed",
			From:     oldF.Default,
			To:       newF.Default,
		})
	}

	// Backfill expression changes. The modifier is metadata for
	// `tide apply --backfill`; the schema itself doesn't change so every
	// transition is additive. A paired NOT-NULL tightening on the same
	// field still gets classified BackfillRequired by the existing
	// branches above — that's what gates plain `tide apply`.
	switch {
	case oldF.Backfill == "" && newF.Backfill != "":
		d.append(Change{
			Kind:     KindFieldBackfillAdded,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   fmt.Sprintf("backfill expression added: %s", newF.Backfill),
			To:       newF.Backfill,
		})
	case oldF.Backfill != "" && newF.Backfill == "":
		d.append(Change{
			Kind:     KindFieldBackfillRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "backfill expression removed",
			From:     oldF.Backfill,
		})
	case oldF.Backfill != "" && newF.Backfill != "" && oldF.Backfill != newF.Backfill:
		d.append(Change{
			Kind:     KindFieldBackfillChanged,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "backfill expression changed",
			From:     oldF.Backfill,
			To:       newF.Backfill,
		})
	}

	// SERIAL (BIGSERIAL) changes. Both directions are backfill-required
	// because either side needs out-of-band coordination: adding serial
	// needs the sequence seeded to MAX(col)+1 so the next auto-generated
	// id doesn't collide with existing rows; removing serial means every
	// caller that relied on auto-increment must now supply the column
	// explicitly on INSERT, which is a behaviour change verified by
	// hand rather than auto-detected by the diff engine.
	switch {
	case !oldF.Serial && newF.Serial:
		d.append(Change{
			Kind:     KindFieldSerialAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "SERIAL added — seed sequence to MAX(col)+1 before apply",
		})
	case oldF.Serial && !newF.Serial:
		d.append(Change{
			Kind:     KindFieldSerialRemoved,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "SERIAL removed — callers must now supply the column explicitly",
		})
	}

	// Reference changes.
	switch {
	case oldF.Ref == nil && newF.Ref != nil:
		// Adding an FK to an existing column may fail if existing rows violate
		// it — backfill-required.
		d.append(Change{
			Kind:     KindFieldReferenceAdded,
			Class:    ClassBackfillRequired,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "FK added to existing column — verify referential integrity",
			To:       newF.Ref,
		})
	case oldF.Ref != nil && newF.Ref == nil:
		d.Additive = append(d.Additive, Change{
			Kind:     KindFieldReferenceRemoved,
			Class:    ClassAdditive,
			EntityID: entityID,
			Field:    newF.Name,
			Detail:   "FK removed",
			From:     oldF.Ref,
		})
	case oldF.Ref != nil && newF.Ref != nil && !refsEqual(oldF.Ref, newF.Ref):
		// Distinguish three sub-cases:
		//   - Target entity/field changed       → breaking (callers may
		//     have integrity assumptions about the *target* row)
		//   - Action weakened (cascade ← restrict, set null ← restrict, …)
		//     → additive: existing operations that succeeded still succeed
		//   - Action strengthened (restrict ← cascade) → backfill: existing
		//     dependent rows may now block writes that previously cascaded
		switch {
		case oldF.Ref.TargetID != newF.Ref.TargetID || oldF.Ref.TargetField != newF.Ref.TargetField:
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassCrossCallerBreaking,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK target changed",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		case refActionStrengthened(oldF.Ref.OnDelete, newF.Ref.OnDelete) ||
			refActionStrengthened(oldF.Ref.OnUpdate, newF.Ref.OnUpdate):
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassBackfillRequired,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK action strengthened — verify dependent rows",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		default:
			d.append(Change{
				Kind:     KindFieldReferenceModified,
				Class:    ClassAdditive,
				EntityID: entityID,
				Field:    newF.Name,
				Detail:   "FK action weakened",
				From:     oldF.Ref,
				To:       newF.Ref,
			})
		}
	}
}

func diffIndexes(oldE, newE *dsl.Entity, d *Diff) {
	oldKeys := indexKeys(oldE.Indexes)
	newKeys := indexKeys(newE.Indexes)
	for k, idx := range newKeys {
		if _, ok := oldKeys[k]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind:     KindIndexAdded,
				Class:    ClassAdditive,
				EntityID: newE.ID(),
				Detail:   "index added: " + k,
				To:       idx,
			})
		}
	}
	for k, idx := range oldKeys {
		if _, ok := newKeys[k]; !ok {
			d.Additive = append(d.Additive, Change{
				Kind:     KindIndexRemoved,
				Class:    ClassAdditive,
				EntityID: newE.ID(),
				Detail:   "index removed: " + k,
				From:     idx,
			})
		}
	}
}

func diffCache(oldE, newE *dsl.Entity, d *Diff) {
	if cacheEqual(oldE.Cache, newE.Cache) {
		return
	}
	// Cache changes are always additive — they affect server behavior, not
	// the wire contract or stored data.
	d.Additive = append(d.Additive, Change{
		Kind:     KindCacheChanged,
		Class:    ClassAdditive,
		EntityID: newE.ID(),
		Detail:   "cache config changed",
		From:     oldE.Cache,
		To:       newE.Cache,
	})
}

func diffQueryTimeout(oldE, newE *dsl.Entity, d *Diff) {
	if oldE.QueryTimeoutMS == newE.QueryTimeoutMS {
		return
	}
	d.Additive = append(d.Additive, Change{
		Kind:     KindQueryTimeoutChanged,
		Class:    ClassAdditive,
		EntityID: newE.ID(),
		Detail:   fmt.Sprintf("query_timeout %dms → %dms", oldE.QueryTimeoutMS, newE.QueryTimeoutMS),
		From:     oldE.QueryTimeoutMS,
		To:       newE.QueryTimeoutMS,
	})
}

// ---- helpers ----

func (d *Diff) append(c Change) {
	switch c.Class {
	case ClassAdditive:
		d.Additive = append(d.Additive, c)
	case ClassBackfillRequired:
		d.BackfillRequired = append(d.BackfillRequired, c)
	case ClassCrossCallerBreaking:
		d.Breaking = append(d.Breaking, c)
	}
}

func indexByID(ir *dsl.IR) map[string]*dsl.Entity {
	out := map[string]*dsl.Entity{}
	if ir == nil {
		return out
	}
	for i := range ir.Entities {
		out[ir.Entities[i].ID()] = &ir.Entities[i]
	}
	return out
}

func fieldsByName(e *dsl.Entity) map[string]*dsl.Field {
	out := map[string]*dsl.Field{}
	for i := range e.Fields {
		out[e.Fields[i].Name] = &e.Fields[i]
	}
	return out
}

func mergedNames[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	sort.Strings(out)
	return out
}

// typeEqual compares two field types structurally.
func typeEqual(a, b dsl.FieldType) bool {
	if a.Name != b.Name || a.Array != b.Array || a.VecDim != b.VecDim ||
		a.NumP != b.NumP || a.NumS != b.NumS || a.HasNumP != b.HasNumP {
		return false
	}
	if a.Array {
		if a.Elem == nil || b.Elem == nil {
			return a.Elem == b.Elem
		}
		return typeEqual(*a.Elem, *b.Elem)
	}
	return true
}

// typeString renders a type for human-readable diff details.
func typeString(t dsl.FieldType) string {
	if t.Array {
		if t.Elem != nil {
			return "[]" + typeString(*t.Elem)
		}
		return "[]" + t.Name
	}
	switch t.Name {
	case "vector":
		return fmt.Sprintf("vector(%d)", t.VecDim)
	case "numeric":
		if t.HasNumP {
			return fmt.Sprintf("numeric(%d,%d)", t.NumP, t.NumS)
		}
		return "numeric"
	}
	return t.Name
}

// classifyTypeChange decides whether moving from oldT to newT is safe (additive),
// requires backfill, or is breaking.
//
// Conservative rules:
//   - Same name with widening (smallint→int→bigint, int→bigint): additive
//   - Same name with narrowing (bigint→int): backfill-required
//   - Numeric precision increase: additive; decrease: backfill-required
//   - Vector dimension change: breaking (data must be re-embedded)
//   - Array vs scalar: breaking
//   - Anything else: backfill-required (force human review)
func classifyTypeChange(oldT, newT dsl.FieldType) ChangeClass {
	if oldT.Array != newT.Array {
		return ClassCrossCallerBreaking
	}
	if oldT.Name == "vector" && newT.Name == "vector" {
		if oldT.VecDim != newT.VecDim {
			return ClassCrossCallerBreaking
		}
		return ClassAdditive
	}
	if oldT.Name == newT.Name {
		// numeric precision/scale
		if oldT.Name == "numeric" {
			if newT.NumP < oldT.NumP || newT.NumS < oldT.NumS {
				return ClassBackfillRequired
			}
			return ClassAdditive
		}
		return ClassAdditive
	}
	if isWidening(oldT.Name, newT.Name) {
		return ClassAdditive
	}
	if isNarrowing(oldT.Name, newT.Name) {
		return ClassBackfillRequired
	}
	// Different families (text → int, jsonb → text, etc.) — force review.
	return ClassBackfillRequired
}

var intRank = map[string]int{
	"smallint": 1,
	"int":      2,
	"bigint":   3,
}

func isWidening(oldName, newName string) bool {
	o, ok1 := intRank[oldName]
	n, ok2 := intRank[newName]
	return ok1 && ok2 && n > o
}

func isNarrowing(oldName, newName string) bool {
	o, ok1 := intRank[oldName]
	n, ok2 := intRank[newName]
	return ok1 && ok2 && n < o
}

func defaultsEqual(a, b *dsl.Default) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// refActionStrengthened returns true iff going from `old` to `new` would
// reject operations that previously succeeded. Order from "permissive" to
// "strict":
//
//	cascade < set null < restrict
//
// Adding a previously-unset action (RefActionUnset → anything) counts as a
// strengthening because the prior behavior was Postgres's default (NO
// ACTION, which equates to RESTRICT at commit). Moving in the other
// direction is a weakening.
func refActionStrengthened(old, newAct dsl.RefAction) bool {
	rank := func(a dsl.RefAction) int {
		switch a {
		case dsl.RefActionCascade:
			return 1
		case dsl.RefActionSetNull:
			return 2
		case dsl.RefActionRestrict, dsl.RefActionUnset:
			return 3 // unset == NO ACTION, behaviorally RESTRICT-ish
		}
		return 3
	}
	return rank(newAct) > rank(old)
}

func refsEqual(a, b *dsl.Ref) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// indexFieldKey returns a stable token representing one IndexField (either
// a column name or an expression). Used inside indexKey for diff stability.
func indexFieldKey(f dsl.IndexField) string {
	if f.IsExpr {
		return "expr:" + f.Expr
	}
	return f.Name
}

// indexKeys returns a stable identifier for each index in the slice. Indexes
// are compared by structural shape; reordering them in the .atl file produces
// no diff.
func indexKeys(idxs []dsl.Index) map[string]dsl.Index {
	out := map[string]dsl.Index{}
	for _, idx := range idxs {
		out[indexKey(idx)] = idx
	}
	return out
}

func indexKey(idx dsl.Index) string {
	switch idx.Kind {
	case dsl.IndexBtree:
		s := "btree:"
		for i, f := range idx.Fields {
			if i > 0 {
				s += ","
			}
			s += indexFieldKey(f)
			if f.Desc {
				s += " desc"
			}
		}
		return s
	case dsl.IndexPartial:
		s := "partial:"
		for i, f := range idx.Fields {
			if i > 0 {
				s += ","
			}
			s += indexFieldKey(f)
			if f.Desc {
				s += " desc"
			}
		}
		if idx.Where != nil {
			s += "|" + idx.Where.Field
			switch {
			case idx.Where.Op == "" && idx.Where.IsNull:
				s += " is null"
			case idx.Where.Op == "":
				s += " is not null"
			default:
				s += " " + idx.Where.Op
				if idx.Where.Literal != nil {
					switch idx.Where.Literal.Kind {
					case dsl.DefaultIRString:
						s += " " + idx.Where.Literal.Str
					case dsl.DefaultIRInt:
						s += fmt.Sprintf(" %d", idx.Where.Literal.Int)
					case dsl.DefaultIRBool:
						s += fmt.Sprintf(" %v", idx.Where.Literal.Bool)
					}
				}
			}
		}
		return s
	case dsl.IndexHNSW:
		return fmt.Sprintf("hnsw:%s:%s", idx.Field, idx.VecOps)
	case dsl.IndexGIN:
		return "gin:" + idx.Field
	}
	return "unknown"
}

func cacheEqual(a, b *dsl.Cache) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.HasReadThrough != b.HasReadThrough || a.TTLMS != b.TTLMS ||
		a.Tag != b.Tag || a.Consistency != b.Consistency {
		return false
	}
	if !sliceEq(a.TagFields, b.TagFields) {
		return false
	}
	if len(a.Invalidate) != len(b.Invalidate) {
		return false
	}
	for i := range a.Invalidate {
		ai, bi := a.Invalidate[i], b.Invalidate[i]
		if ai.Self != bi.Self || ai.TargetID != bi.TargetID {
			return false
		}
		if (ai.Where == nil) != (bi.Where == nil) {
			return false
		}
		if ai.Where != nil && *ai.Where != *bi.Where {
			return false
		}
	}
	return true
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
