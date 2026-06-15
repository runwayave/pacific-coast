package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/predsql"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// EmitSQL turns a Diff into a pair of migration scripts.
//
// Design choices:
//   - One file per call (caller decides the filename / sequence number).
//   - Up and down are returned as separate strings; the caller writes them
//     to disk side-by-side (NNNN_<name>.up.sql, NNNN_<name>.down.sql).
//   - Entities default to the `atlantis` schema; the `table "schema.table"`
//     override routes them elsewhere, and the emitter issues
//     CREATE SCHEMA IF NOT EXISTS for each non-default target before any
//     CREATE TABLE.
//   - Reversibility: every up has a matching down. CI runs up/down/up
//     against a fresh DB.
//
// Caveats accepted:
//   - We do NOT auto-generate backfill SQL — that's a separate file the
//     engineer supplies (--backfill).
//   - Cache and query_timeout changes do not produce SQL (they affect server
//     behavior only). They are reflected in the script as a no-op comment so
//     the file isn't empty when those are the only changes.
//   - Schema-qualified names throughout (atlantis.* by default; the
//     schema from `table "schema.table"` when overridden), with no
//     search_path manipulation, so the emitted SQL is unambiguous
//     regardless of the caller's session search_path.
//
// SQLScripts is the migration output of one emit pass. Up + Down are the
// legacy single-script forms — what plain `tide apply` consumes. The four
// PreBackfill / PostBackfill fields and BackfillFields are populated when
// the diff contains at least one field with a `backfill` modifier paired
// with a NOT-NULL change; `tide apply --backfill` runs them in order
// around the chunked UPDATE loop:
//
//	PreBackfillUp       — additive parts + ADD COLUMN nullable for backfilled fields
//	PreBackfillIndexes  — CREATE INDEX CONCURRENTLY ... WHERE field IS NULL
//	(chunked UPDATE loop runs here, driven by BackfillFields)
//	PostBackfillUp      — ALTER COLUMN SET NOT NULL on backfilled fields
//	PostBackfillIndexes — DROP INDEX CONCURRENTLY (mirror of the partial idx)
//
// For non-backfill plans the four scripts are empty strings and
// BackfillFields is nil — callers treat "PostBackfillUp == ”" as the
// no-phase-split signal.
type SQLScripts struct {
	Up   string
	Down string

	PreBackfillUp       string
	PreBackfillIndexes  string
	PostBackfillUp      string
	PostBackfillIndexes string

	BackfillFields []BackfillField
}

// BackfillField is one entry in the chunked-UPDATE driver list. The admin
// RPC turns these into atlantis.backfill_field_state rows that the
// background worker drains. TableName is schema-qualified and pre-quoted
// so the splicer can embed it verbatim.
type BackfillField struct {
	EntityID   string `json:"entity_id"`
	Field      string `json:"field"`
	Expression string `json:"expression"`
	PKColumn   string `json:"pk_column"`
	TableName  string `json:"table_name"`
}

// EmitSQL emits one migration covering every change in d, against the new IR
// (newIR is needed to look up entity / field shape for additive changes).
//
// oldIR may be nil (initial migration). newIR must not be nil.
func EmitSQL(oldIR, newIR *dsl.IR, d *Diff) (SQLScripts, error) {
	if newIR == nil {
		return SQLScripts{}, fmt.Errorf("EmitSQL: newIR is required")
	}
	newByID := indexByID(newIR)
	oldByID := indexByID(oldIR)

	up := &sqlBuilder{}
	down := &sqlBuilder{}

	// Static banner. Per-change banners come from emitClass below.
	up.line("-- atlantis migration (generated)")
	up.line("-- DO NOT EDIT BY HAND. Re-run `tide plan` after editing .atl files.")
	up.blank()
	down.line("-- atlantis migration (generated, down)")
	down.line("-- DO NOT EDIT BY HAND.")
	down.blank()

	// Ensure every Postgres schema referenced by a newly-added entity exists
	// before any CREATE TABLE in that schema runs. CREATE SCHEMA IF NOT
	// EXISTS is idempotent, so re-emitting it on a follow-up migration that
	// happens to add another entity to the same schema is harmless. The
	// `atlantis` and `public` schemas are skipped — `atlantis` is created
	// by the infra migrations at server boot, and `public` always exists.
	// Down does NOT drop schemas: a schema may hold non-atlantis objects,
	// and a rollback that leaves an empty schema is a tiny, harmless
	// artifact compared to risking data loss in unrelated tables.
	for _, name := range collectNewSchemas(d.Additive, newByID) {
		up.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	if len(collectNewSchemas(d.Additive, newByID)) > 0 {
		up.blank()
	}

	// Process all changes in a deterministic order. We split by class so the
	// reader sees additive first, then any backfill-required changes (with
	// big comment banners), then breaking changes (likewise).
	emitClass(up, down, "ADDITIVE", d.Additive, newByID, oldByID)
	emitClass(up, down, "BACKFILL REQUIRED", d.BackfillRequired, newByID, oldByID)
	emitClass(up, down, "BREAKING — REVIEW CAREFULLY", d.Breaking, newByID, oldByID)

	// If nothing was emitted, the migration is genuinely empty (e.g., only
	// cache changes). Leave a comment so the file isn't blank.
	if d.IsEmpty() {
		up.line("-- (no schema changes)")
		down.line("-- (no schema changes)")
	}

	scripts := SQLScripts{Up: up.String(), Down: down.String()}
	// Phase-split scripts are emitted in a separate pass because they
	// have different routing rules (NOT NULL deferred to post, partial
	// index on the NULL set bracketing the chunked backfill loop).
	// Populated only when the diff contains a backfilled field paired
	// with a NOT NULL tightening or new-NOT-NULL — otherwise the four
	// PreBackfill* / PostBackfill* fields stay empty and callers fall
	// through to plain `tide apply`.
	if needsPhaseSplit(d, newByID) {
		pre, preIdx, post, postIdx, fields := buildPhaseSplit(d, newByID, oldByID)
		scripts.PreBackfillUp = pre.String()
		scripts.PreBackfillIndexes = preIdx.String()
		scripts.PostBackfillUp = post.String()
		scripts.PostBackfillIndexes = postIdx.String()
		scripts.BackfillFields = fields
	}
	return scripts, nil
}

// collectRequiredExtensions walks newIR and returns the sorted set of
// Postgres extension names the schema depends on. Triggers:
//
//	vector(N) column      → vector
//	hypertable entity     → timescaledb
//	citext column         → citext
//
// Used by EmitInitial to emit only the CREATE EXTENSION calls the schema
// actually needs. The server's apply-time auto-enable path
// (internal/server/admin/extensions.go) uses the same trigger list to
// drive pg_available_extensions / pg_extension checks at apply time.
func collectRequiredExtensions(newIR *dsl.IR) []string {
	seen := map[string]struct{}{}
	add := func(name string) { seen[name] = struct{}{} }
	for i := range newIR.Entities {
		e := &newIR.Entities[i]
		if e.Kind == dsl.EntityKindHypertable {
			add("timescaledb")
		}
		for j := range e.Fields {
			collectFieldExtensions(&e.Fields[j].Type, add)
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// collectFieldExtensions descends through Array/Elem so a hypothetical
// `[]vector(N)` still triggers pgvector.
func collectFieldExtensions(t *dsl.FieldType, add func(string)) {
	if t == nil {
		return
	}
	switch t.Name {
	case "vector":
		add("vector")
	case "citext":
		add("citext")
	}
	if t.Array && t.Elem != nil {
		collectFieldExtensions(t.Elem, add)
	}
}

// collectInitialSchemas returns every distinct Postgres schema (other than
// "atlantis" and "public") referenced by entities in newIR — sorted for
// deterministic output. Used by EmitInitial to provision schemas for
// `table "schema.table"` overrides ahead of CREATE TABLE.
func collectInitialSchemas(newIR *dsl.IR) []string {
	seen := map[string]struct{}{}
	for i := range newIR.Entities {
		s := entitySchema(&newIR.Entities[i])
		if s == "atlantis" || s == "public" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// collectNewSchemas walks the EntityAdded changes in additive and returns
// every distinct Postgres schema (other than "atlantis" and "public") that
// the new entities will land in — sorted for deterministic output. Used to
// emit `CREATE SCHEMA IF NOT EXISTS` headers ahead of the first CREATE
// TABLE in each non-default schema.
func collectNewSchemas(additive []Change, newByID map[string]*dsl.Entity) []string {
	seen := map[string]struct{}{}
	for _, ch := range additive {
		if ch.Kind != KindEntityAdded {
			continue
		}
		e := newByID[ch.EntityID]
		if e == nil {
			continue
		}
		s := entitySchema(e)
		if s == "atlantis" || s == "public" {
			continue
		}
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// needsPhaseSplit returns true iff the diff requires the apply to split
// around a chunked backfill — i.e., a field has Backfill != "" and is
// involved in a NOT NULL tightening or new-NOT-NULL change.
func needsPhaseSplit(d *Diff, newByID map[string]*dsl.Entity) bool {
	for _, ch := range d.BackfillRequired {
		if isBackfilledFieldInDiff(ch, newByID) {
			return true
		}
	}
	for _, ch := range d.Additive {
		if isBackfilledFieldInDiff(ch, newByID) {
			return true
		}
	}
	return false
}

func isBackfilledFieldInDiff(ch Change, newByID map[string]*dsl.Entity) bool {
	if ch.Kind != KindFieldAdded && ch.Kind != KindFieldNotNullTightened {
		return false
	}
	e := newByID[ch.EntityID]
	if e == nil {
		return false
	}
	f := e.FindField(ch.Field)
	if f == nil || f.Backfill == "" {
		return false
	}
	// FieldAdded counts only when the new field is NOT NULL — otherwise
	// there's no constraint to defer.
	if ch.Kind == KindFieldAdded && !f.NotNull {
		return false
	}
	return true
}

// buildPhaseSplit walks d once more and emits the four phase-split
// scripts. Non-backfill changes go to pre verbatim (via emitChange). For
// each backfilled field with a NOT NULL change: the ADD COLUMN body goes
// to pre as nullable, the SET NOT NULL goes to post, and a partial index
// on the NULL set is created in preIdx + dropped in postIdx.
//
// Down emission is not phase-split — a rollback of a backfill plan is a
// manual operator concern; the legacy Down covers it.
func buildPhaseSplit(d *Diff, newByID, oldByID map[string]*dsl.Entity) (pre, preIdx, post, postIdx *sqlBuilder, fields []BackfillField) {
	pre = &sqlBuilder{}
	preIdx = &sqlBuilder{}
	post = &sqlBuilder{}
	postIdx = &sqlBuilder{}

	pre.line("-- atlantis migration (pre-backfill phase)")
	pre.line("-- Runs in the apply tx before `tide apply --backfill` kicks off the chunked UPDATE.")
	pre.blank()

	// Mirror EmitSQL's CREATE SCHEMA prelude in the phase-split path so a
	// backfill plan whose additive part introduces a new namespace can also
	// boot from a clean DB.
	for _, name := range collectNewSchemas(d.Additive, newByID) {
		pre.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	if len(collectNewSchemas(d.Additive, newByID)) > 0 {
		pre.blank()
	}
	post.line("-- atlantis migration (post-backfill phase)")
	post.line("-- Runs after the chunked backfill completes — applies SET NOT NULL on backfilled fields.")
	post.blank()
	preIdx.line("-- Partial-index lifecycle for the chunked backfill. CREATE INDEX CONCURRENTLY")
	preIdx.line("-- runs OUTSIDE a transaction; each line is its own statement.")
	preIdx.blank()
	postIdx.line("-- Drop the partial indexes created pre-backfill. CONCURRENTLY for parity.")
	postIdx.blank()

	deferred := map[[2]string]*dsl.Entity{}
	throwaway := &sqlBuilder{}

	for _, ch := range d.Additive {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}
	for _, ch := range d.BackfillRequired {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}
	for _, ch := range d.Breaking {
		emitPhaseSplitChange(ch, newByID, oldByID, pre, post, throwaway, deferred)
	}

	for key, e := range deferred {
		entityID, fieldName := key[0], key[1]
		f := e.FindField(fieldName)
		pk := primaryKeyColumn(e)
		if pk == "" || f == nil {
			preIdx.linef("-- SKIPPED: %s has no single-column PK; chunked backfill requires a single-column PK for the cursor", qualifiedTable(e))
			continue
		}
		idxName := backfillIndexName(e, fieldName)
		preIdx.linef("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s) WHERE %s IS NULL;",
			quoteIdent(idxName), qualifiedTable(e), quoteIdent(pk), quoteIdent(fieldName))
		postIdx.linef(`DROP INDEX CONCURRENTLY IF EXISTS %s.%s;`, quoteIdent(entitySchema(e)), quoteIdent(idxName))
		fields = append(fields, BackfillField{
			EntityID:   entityID,
			Field:      fieldName,
			Expression: f.Backfill,
			PKColumn:   pk,
			TableName:  qualifiedTable(e),
		})
	}
	return pre, preIdx, post, postIdx, fields
}

// emitPhaseSplitChange emits one change into the pre/post builders,
// routing NOT NULL on backfilled fields to post. throwaway absorbs the
// "down" output that emitChange always writes — phase split doesn't need
// it.
func emitPhaseSplitChange(ch Change, newByID, oldByID map[string]*dsl.Entity, pre, post, throwaway *sqlBuilder, deferred map[[2]string]*dsl.Entity) {
	e := newByID[ch.EntityID]
	if e == nil {
		emitChange(pre, throwaway, ch, newByID, oldByID)
		return
	}
	switch ch.Kind {
	case KindFieldAdded:
		f := e.FindField(ch.Field)
		if f != nil && f.Backfill != "" && f.NotNull {
			pre.linef("-- %s: %s (backfill-deferred; ADD nullable here, SET NOT NULL in post)", ch.Kind, ch.Detail)
			emitFieldAddNullable(pre, e, f)
			pre.blank()
			post.linef("-- %s: %s (post-backfill SET NOT NULL)", ch.Kind, ch.Detail)
			emitNotNull(post, e, ch.Field, true)
			post.blank()
			deferred[[2]string{ch.EntityID, ch.Field}] = e
			return
		}
		emitChange(pre, throwaway, ch, newByID, oldByID)
	case KindFieldNotNullTightened:
		f := e.FindField(ch.Field)
		if f != nil && f.Backfill != "" {
			post.linef("-- %s: %s (post-backfill SET NOT NULL)", ch.Kind, ch.Detail)
			emitNotNull(post, e, ch.Field, true)
			post.blank()
			deferred[[2]string{ch.EntityID, ch.Field}] = e
			return
		}
		emitChange(pre, throwaway, ch, newByID, oldByID)
	default:
		emitChange(pre, throwaway, ch, newByID, oldByID)
	}
}

// emitFieldAddNullable adds the column with NOT NULL suppressed, even if
// f.NotNull is true. Used by the phase-split builder so the column is
// nullable while the chunked backfill populates it; Phase 3 then runs
// SET NOT NULL.
func emitFieldAddNullable(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	nullable := *f
	nullable.NotNull = false
	b.linef("ALTER TABLE %s ADD COLUMN %s;", qualifiedTable(e), columnDecl(nullable))
	if f.Ref != nil {
		emitFKAdd(b, e, f)
	}
}

// backfillIndexName names the partial index Phase 1 creates and Phase 3
// drops. Shape mirrors the existing pkName / fkName / uqName helpers.
func backfillIndexName(e *dsl.Entity, fieldName string) string {
	return tableName(e) + "_" + fieldName + "_backfill_idx"
}

// primaryKeyColumn returns the single PK column name, or "" for a
// composite-PK entity. The chunked-backfill driver only supports
// single-column cursors today.
func primaryKeyColumn(e *dsl.Entity) string {
	if pf := e.PrimaryField(); pf != nil {
		return pf.Name
	}
	return ""
}

func emitClass(up, down *sqlBuilder, label string, changes []Change, newByID, oldByID map[string]*dsl.Entity) {
	if len(changes) == 0 {
		return
	}
	up.line("-- ==== " + label + " ====")
	down.line("-- ==== " + label + " ====")
	for _, ch := range reorderEntityAddsByFKDependency(changes, newByID) {
		emitChange(up, down, ch, newByID, oldByID)
	}
	up.blank()
	down.blank()
}

// reorderEntityAddsByFKDependency returns changes with EntityAdded entries
// topologically sorted ahead of all other changes, so an FK from a new
// entity to another new entity in the same migration resolves correctly
// regardless of the entities' alphabetical order. Non-EntityAdded changes
// (field add, NOT NULL tighten, …) target entities that already exist in
// the prior schema, so their ordering relative to each other is preserved.
// On a topo failure (true cycle between two different new entities), the
// original order is returned so the apply fails with a clearer Postgres
// error rather than swallowing the change set.
func reorderEntityAddsByFKDependency(changes []Change, newByID map[string]*dsl.Entity) []Change {
	var adds []Change
	var rest []Change
	for _, ch := range changes {
		if ch.Kind == KindEntityAdded {
			adds = append(adds, ch)
		} else {
			rest = append(rest, ch)
		}
	}
	if len(adds) <= 1 {
		return changes
	}

	entities := make([]dsl.Entity, 0, len(adds))
	for _, ch := range adds {
		if e := newByID[ch.EntityID]; e != nil {
			entities = append(entities, *e)
		}
	}
	sorted, err := topoSortEntities(entities)
	if err != nil {
		return changes
	}

	addByID := map[string]Change{}
	for _, ch := range adds {
		addByID[ch.EntityID] = ch
	}
	out := make([]Change, 0, len(changes))
	for _, e := range sorted {
		out = append(out, addByID[e.ID()])
	}
	out = append(out, rest...)
	return out
}

// emitChange dispatches one change to its specific emitter.
//
// Down statements are written in REVERSE structural order on the down side by
// each emitter — i.e. a CREATE TABLE on up is mirrored by DROP TABLE on down,
// and a column ADD is mirrored by a column DROP. The orchestration outer
// loop preserves the additive→backfill→breaking sequence on the up side; the
// down side mirrors that. (We don't re-sort the down statements —
// migrate runs them top-to-bottom as written, and the per-change reversal is
// sufficient because we never combine destructive + additive changes in the
// same migration without explicit ceremony.)
func emitChange(up, down *sqlBuilder, ch Change, newByID, oldByID map[string]*dsl.Entity) {
	up.linef("-- %s: %s", ch.Kind, ch.Detail)
	down.linef("-- %s (reversed): %s", ch.Kind, ch.Detail)
	switch ch.Kind {
	case KindEntityAdded:
		e := newByID[ch.EntityID]
		emitEntityCreate(up, e)
		emitEntityDrop(down, e)
	case KindEntityRemoved:
		e := oldByID[ch.EntityID]
		emitEntityDrop(up, e)
		emitEntityCreate(down, e)
	case KindFieldAdded:
		e := newByID[ch.EntityID]
		f := e.FindField(ch.Field)
		emitFieldAdd(up, e, f)
		emitFieldDrop(down, e, f.Name)
	case KindFieldRemoved:
		oldE := oldByID[ch.EntityID]
		f := oldE.FindField(ch.Field)
		emitFieldDrop(up, oldE, f.Name)
		emitFieldAdd(down, oldE, f)
	case KindFieldNotNullTightened:
		e := newByID[ch.EntityID]
		emitNotNull(up, e, ch.Field, true)
		emitNotNull(down, e, ch.Field, false)
	case KindFieldNotNullLoosened:
		e := newByID[ch.EntityID]
		emitNotNull(up, e, ch.Field, false)
		emitNotNull(down, e, ch.Field, true)
	case KindFieldTypeChanged:
		e := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		fromT := oldE.FindField(ch.Field).Type
		toT := e.FindField(ch.Field).Type
		emitTypeChange(up, e, ch.Field, toT)
		emitTypeChange(down, oldE, ch.Field, fromT)
	case KindFieldDefaultChanged:
		e := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		emitDefault(up, e, ch.Field, e.FindField(ch.Field).Default)
		emitDefault(down, oldE, ch.Field, oldE.FindField(ch.Field).Default)
	case KindFieldUniqueAdded:
		e := newByID[ch.EntityID]
		emitUnique(up, e, ch.Field, true)
		emitUnique(down, e, ch.Field, false)
	case KindFieldUniqueRemoved:
		e := newByID[ch.EntityID]
		emitUnique(up, e, ch.Field, false)
		emitUnique(down, e, ch.Field, true)
	case KindCompositeUniqueAdded:
		u, _ := ch.To.(dsl.UniqueSpec)
		e := newByID[ch.EntityID]
		emitCompositeUnique(up, e, u.Fields, true)
		emitCompositeUnique(down, e, u.Fields, false)
	case KindCompositeUniqueRemoved:
		u, _ := ch.From.(dsl.UniqueSpec)
		e := newByID[ch.EntityID]
		if e == nil {
			e = oldByID[ch.EntityID]
		}
		emitCompositeUnique(up, e, u.Fields, false)
		emitCompositeUnique(down, e, u.Fields, true)
	case KindFieldReferenceAdded:
		e := newByID[ch.EntityID]
		f := e.FindField(ch.Field)
		emitFKAdd(up, e, f)
		emitFKDrop(down, e, f)
	case KindFieldReferenceRemoved:
		oldE := oldByID[ch.EntityID]
		f := oldE.FindField(ch.Field)
		emitFKDrop(up, oldE, f)
		emitFKAdd(down, oldE, f)
	case KindFieldReferenceModified:
		newE := newByID[ch.EntityID]
		oldE := oldByID[ch.EntityID]
		newF := newE.FindField(ch.Field)
		oldF := oldE.FindField(ch.Field)
		// Drop old FK, add new one.
		emitFKDrop(up, oldE, oldF)
		emitFKAdd(up, newE, newF)
		emitFKDrop(down, newE, newF)
		emitFKAdd(down, oldE, oldF)
	case KindIndexAdded:
		idx, _ := ch.To.(dsl.Index)
		e := newByID[ch.EntityID]
		emitIndexCreate(up, e, idx)
		emitIndexDrop(down, e, idx)
	case KindIndexRemoved:
		idx, _ := ch.From.(dsl.Index)
		e := newByID[ch.EntityID]
		if e == nil {
			e = oldByID[ch.EntityID]
		}
		emitIndexDrop(up, e, idx)
		emitIndexCreate(down, e, idx)
	case KindCacheChanged, KindQueryTimeoutChanged:
		up.line("-- (no SQL: cache / query_timeout are server-side)")
		down.line("-- (no SQL: cache / query_timeout are server-side)")
	case KindCustomQueryAdded, KindCustomQueryChanged, KindCustomQueryRemoved,
		KindProcedureAdded, KindProcedureChanged, KindProcedureRemoved:
		// Custom queries/procedures are served at runtime from the
		// checkpoint IR, not migrated — no DDL. The comment keeps the
		// migration file self-documenting and stops a custom-only apply
		// from rendering as "(no schema changes)".
		up.linef("-- (no SQL: %s)", ch.Detail)
		down.linef("-- (no SQL: %s)", ch.Detail)
	case KindFieldBackfillAdded, KindFieldBackfillRemoved, KindFieldBackfillChanged:
		// The backfill modifier is metadata for `tide apply --backfill`;
		// the schema doesn't change so no SQL is emitted in the legacy
		// up/down. Phase-split scripts are populated separately in
		// buildPhaseSplit.
		up.line("-- (no SQL: backfill modifier is metadata for tide apply --backfill)")
		down.line("-- (no SQL: backfill modifier is metadata for tide apply --backfill)")
	case KindFieldSerialAdded, KindFieldSerialRemoved:
		// Serial flips need operator coordination (sequence seed or caller
		// behavior verification); no auto-SQL today.
		up.line("-- (no SQL: serial flip requires explicit operator coordination)")
		down.line("-- (no SQL: serial flip requires explicit operator coordination)")
	case KindEntityTableChanged:
		// Atlantis won't auto-rename a physical table; if the `table "..."`
		// value moved, the operator runs ALTER TABLE RENAME themselves
		// before re-applying. We surface the intent as a comment so the
		// migration file is self-explaining.
		up.linef("-- TABLE NAME CHANGED — manual ALTER TABLE RENAME required: %s", ch.Detail)
		down.linef("-- TABLE NAME CHANGED — manual ALTER TABLE RENAME required: %s", ch.Detail)
	}
	up.blank()
	down.blank()
}

// emitTouchTrigger writes the per-entity BEFORE UPDATE trigger + its
// trigger function. We emit a dedicated function per entity rather than
// one shared function across the schema because the trigger function hardcodes
// the column name (`NEW.updated_at = now()`), and reaching for hstore /
// dynamic SQL to parameterize that adds an extension dependency. Per-entity
// functions are a few extra bytes of DDL each but cost nothing at runtime
// (Postgres caches the plan).
func emitTouchTrigger(b *sqlBuilder, e *dsl.Entity) {
	if e.TouchOnUpdateField == "" {
		return
	}
	fnName := triggerName(e, "touch_fn")
	trName := triggerName(e, "touch")
	col := e.TouchOnUpdateField

	// Function — OR REPLACE makes this idempotent on re-runs.
	b.linef(`CREATE OR REPLACE FUNCTION "atlantis".%s() RETURNS TRIGGER AS $$`, quoteIdent(fnName))
	b.line(`BEGIN`)
	b.linef(`  NEW.%s = now();`, quoteIdent(col))
	b.line(`  RETURN NEW;`)
	b.line(`END;`)
	b.line(`$$ LANGUAGE plpgsql;`)

	// Trigger — Postgres < 14 doesn't accept CREATE TRIGGER IF NOT EXISTS,
	// so DROP IF EXISTS + CREATE is the cross-version-safe idempotent form.
	b.linef(`DROP TRIGGER IF EXISTS %s ON %s;`, quoteIdent(trName), qualifiedTable(e))
	b.linef(`CREATE TRIGGER %s BEFORE UPDATE ON %s`, quoteIdent(trName), qualifiedTable(e))
	b.linef(`  FOR EACH ROW EXECUTE FUNCTION "atlantis".%s();`, quoteIdent(fnName))
}

func triggerName(e *dsl.Entity, suffix string) string {
	return tableName(e) + "_" + suffix
}

func emitEntityCreate(b *sqlBuilder, e *dsl.Entity) {
	// `IF NOT EXISTS` so the initial migration is idempotent — a
	// partially-failed run (or an out-of-band repair) can re-apply this
	// file without conflict. The diff-driven `tidectl plan` migrations
	// (additive / backfill / breaking) deliberately do NOT carry IF NOT
	// EXISTS so a missing object surfaces as a loud error rather than
	// silent drift.
	b.linef("CREATE TABLE IF NOT EXISTS %s (", qualifiedTable(e))
	cols := []string{}
	var tableConstraints []string
	for _, f := range e.Fields {
		cols = append(cols, "  "+columnDecl(f))
		if f.Ref != nil {
			// Emit FKs as table-level constraints so we can name them
			// deterministically (needed for DROP CONSTRAINT on FK removal).
			tableConstraints = append(tableConstraints, "  "+fkConstraintInline(e, &f))
		}
		if f.Primary && len(e.CompositePK) == 0 {
			tableConstraints = append(tableConstraints,
				fmt.Sprintf("  CONSTRAINT %s PRIMARY KEY (%s)", quoteIdent(pkName(e)), quoteIdent(f.Name)))
		}
	}
	// Composite primary key, if declared. Mutually exclusive with single-
	// field `primary` (the validator enforces this).
	if len(e.CompositePK) > 0 {
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s PRIMARY KEY (%s)",
				quoteIdent(pkName(e)), joinQuoted(e.CompositePK)))
	}
	// Composite UNIQUE constraints — table-level only (single-column UNIQUE
	// stays on the column).
	for _, u := range e.Uniques {
		name := compositeUniqueName(e, u.Fields)
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s UNIQUE (%s)", quoteIdent(name), joinQuoted(u.Fields)))
	}
	// Table-level CHECK constraints (multi-column / polymorphic XOR
	// predicates). The Expr is whatever the engineer wrote inside the
	// `check "..."` declaration; Postgres validates it at migration time.
	for i, c := range e.Checks {
		name := c.Name
		if name == "" {
			name = fmt.Sprintf("%s_check_%d", tableName(e), i+1)
		}
		tableConstraints = append(tableConstraints,
			fmt.Sprintf("  CONSTRAINT %s CHECK (%s)", quoteIdent(name), c.Expr))
	}
	allLines := append(cols, tableConstraints...)
	b.line(strings.Join(allLines, ",\n"))
	b.line(");")

	// Indexes.
	for _, idx := range e.Indexes {
		emitIndexCreate(b, e, idx)
	}

	// Hypertable bootstrap. create_hypertable takes the time column as a
	// quoted string literal (not an identifier), so we double the single
	// quotes for safety in the same way defaultExpr does.
	if e.Kind == dsl.EntityKindHypertable {
		b.linef("SELECT create_hypertable('%s', '%s', if_not_exists => TRUE);",
			qualifiedTable(e), strings.ReplaceAll(e.TimeField, "'", "''"))
	}

	// BEFORE UPDATE auto-touch trigger. Emitted after the
	// table so the table exists at the moment the trigger function
	// references it via CREATE TRIGGER ... ON.
	emitTouchTrigger(b, e)
}

func emitEntityDrop(b *sqlBuilder, e *dsl.Entity) {
	b.linef("DROP TABLE IF EXISTS %s CASCADE;", qualifiedTable(e))
	if e.TouchOnUpdateField != "" {
		// CASCADE on the table drops the trigger itself; the function
		// survives and must be dropped explicitly so the down migration
		// is a true inverse of up.
		fnName := triggerName(e, "touch_fn")
		b.linef(`DROP FUNCTION IF EXISTS "atlantis".%s();`, quoteIdent(fnName))
	}
}

func emitFieldAdd(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s ADD COLUMN %s;", qualifiedTable(e), columnDecl(*f))
	if f.Ref != nil {
		emitFKAdd(b, e, f)
	}
}

func emitFieldDrop(b *sqlBuilder, e *dsl.Entity, name string) {
	b.linef("ALTER TABLE %s DROP COLUMN %s;", qualifiedTable(e), quoteIdent(name))
}

func emitNotNull(b *sqlBuilder, e *dsl.Entity, field string, on bool) {
	op := "SET"
	if !on {
		op = "DROP"
	}
	b.linef("ALTER TABLE %s ALTER COLUMN %s %s NOT NULL;", qualifiedTable(e), quoteIdent(field), op)
}

func emitTypeChange(b *sqlBuilder, e *dsl.Entity, field string, t dsl.FieldType) {
	b.linef("ALTER TABLE %s ALTER COLUMN %s TYPE %s;", qualifiedTable(e), quoteIdent(field), sqlType(t))
}

func emitDefault(b *sqlBuilder, e *dsl.Entity, field string, d *dsl.Default) {
	if d == nil {
		b.linef("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;", qualifiedTable(e), quoteIdent(field))
		return
	}
	b.linef("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;", qualifiedTable(e), quoteIdent(field), defaultExpr(*d))
}

// emitCompositeUnique adds or drops a multi-column UNIQUE constraint. The
// name is deterministic (compositeUniqueName) so the DROP on `on=false`
// finds exactly what a prior ADD created.
func emitCompositeUnique(b *sqlBuilder, e *dsl.Entity, fields []string, on bool) {
	name := compositeUniqueName(e, fields)
	if on {
		b.linef("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);", qualifiedTable(e), quoteIdent(name), joinQuoted(fields))
	} else {
		b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", qualifiedTable(e), quoteIdent(name))
	}
}

func emitUnique(b *sqlBuilder, e *dsl.Entity, field string, on bool) {
	name := uqName(e, field)
	if on {
		b.linef("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s);", qualifiedTable(e), quoteIdent(name), quoteIdent(field))
	} else {
		b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", qualifiedTable(e), quoteIdent(name))
	}
}

func emitFKAdd(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s ADD CONSTRAINT %s %s;",
		qualifiedTable(e), quoteIdent(fkName(e, f.Name)), fkConstraintBody(f))
}

func emitFKDrop(b *sqlBuilder, e *dsl.Entity, f *dsl.Field) {
	b.linef("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;",
		qualifiedTable(e), quoteIdent(fkName(e, f.Name)))
}

func emitIndexCreate(b *sqlBuilder, e *dsl.Entity, idx dsl.Index) {
	// `CREATE INDEX IF NOT EXISTS` on both the initial-migration and the
	// diff-driven add-index paths — the index name is deterministic, so a
	// re-run is a no-op rather than an error.
	name := quoteIdent(indexName(e, idx))
	switch idx.Kind {
	case dsl.IndexBtree:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s (%s);",
			name, qualifiedTable(e), indexFieldList(idx.Fields))
	case dsl.IndexPartial:
		// Only the partial form can be unique (Postgres UNIQUE constraints
		// can't be partial) — see the DSL `unique index partial` form.
		unique := ""
		if idx.Unique {
			unique = "UNIQUE "
		}
		where := ""
		if idx.Where != nil {
			where = " WHERE " + predsql.Render(idx.Where)
		}
		b.linef("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s)%s;",
			unique, name, qualifiedTable(e), indexFieldList(idx.Fields), where)
	case dsl.IndexHNSW:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (%s %s);",
			name, qualifiedTable(e), quoteIdent(idx.Field), idx.VecOps)
	case dsl.IndexGIN:
		b.linef("CREATE INDEX IF NOT EXISTS %s ON %s USING gin (%s);",
			name, qualifiedTable(e), quoteIdent(idx.Field))
	}
}

func emitIndexDrop(b *sqlBuilder, e *dsl.Entity, idx dsl.Index) {
	b.linef("DROP INDEX IF EXISTS %s;", qualifiedIndexName(e, idx))
}

// qualifiedTable returns the schema-qualified, double-quoted table name.
// Delegates to the shared schema package.
func qualifiedTable(e *dsl.Entity) string {
	return schema.QualifiedTable(e)
}

// entitySchema returns the schema where this entity's physical objects live.
// Delegates to the shared schema package.
func entitySchema(e *dsl.Entity) string {
	return schema.EntitySchema(e)
}

// quoteIdent wraps a SQL identifier in double quotes.
// Delegates to the shared schema package.
func quoteIdent(s string) string {
	return schema.QuoteIdent(s)
}

// tableName maps an entity to its physical table name.
// Delegates to the shared schema package.
func tableName(e *dsl.Entity) string {
	return schema.TableName(e)
}

// columnDecl renders one column line for CREATE TABLE / ADD COLUMN.
//
// Identity strategy precedence:
//
//	serial   → render `BIGSERIAL` as the type itself (carries the
//	           sequence + NOT NULL + DEFAULT nextval(...) implicitly).
//	identity → render `<type> GENERATED ALWAYS AS IDENTITY`.
//	neither  → render `<type>` with explicit NOT NULL / DEFAULT modifiers.
func columnDecl(f dsl.Field) string {
	var parts []string
	switch {
	case f.Serial:
		// BIGSERIAL replaces both the type and the GENERATED clause.
		parts = []string{quoteIdent(f.Name), "BIGSERIAL"}
	case f.Identity:
		parts = []string{quoteIdent(f.Name), sqlType(f.Type), "GENERATED ALWAYS AS IDENTITY"}
	default:
		parts = []string{quoteIdent(f.Name), sqlType(f.Type)}
		if f.NotNull && !f.Primary {
			parts = append(parts, "NOT NULL")
		}
		if f.Default != nil {
			parts = append(parts, "DEFAULT "+defaultExpr(*f.Default))
		}
	}
	if f.Unique && !f.Primary {
		parts = append(parts, "UNIQUE")
	}
	if f.Check != "" {
		parts = append(parts, "CHECK ("+f.Check+")")
	}
	return strings.Join(parts, " ")
}

// sqlType maps a DSL field type to its Postgres type.
// Delegates to the shared schema package.
func sqlType(t dsl.FieldType) string {
	return schema.SQLType(t)
}

// defaultExpr renders a Default in SQL form.
// Delegates to the shared schema package.
func defaultExpr(d dsl.Default) string {
	return schema.DefaultExpr(d)
}

// fkConstraintInline renders an FK constraint suitable for use inside a
// CREATE TABLE column list (table-level form).
func fkConstraintInline(e *dsl.Entity, f *dsl.Field) string {
	return fmt.Sprintf("CONSTRAINT %s %s",
		quoteIdent(fkName(e, f.Name)), fkConstraintBody(f))
}

// fkConstraintBody renders the FOREIGN KEY ... REFERENCES ... body. Shared
// between inline (CREATE TABLE) and standalone (ALTER TABLE) forms. Every
// identifier — local column, target table, target column — is quoted so
// reserved-word names roundtrip through Postgres unchanged.
//
// Honors a `table "..."` override on the target entity via
// Ref.TargetTableName, populated at IR-lower time. Without the override,
// falls back to `"atlantis"."<flat>"`.
func fkConstraintBody(f *dsl.Field) string {
	target := fkTargetRef(f.Ref)
	out := fmt.Sprintf(`FOREIGN KEY (%s) REFERENCES %s (%s)`,
		quoteIdent(f.Name), target, quoteIdent(f.Ref.TargetField))
	if f.Ref.OnDelete != dsl.RefActionUnset {
		out += " ON DELETE " + f.Ref.OnDelete.String()
	}
	if f.Ref.OnUpdate != dsl.RefActionUnset {
		out += " ON UPDATE " + f.Ref.OnUpdate.String()
	}
	return out
}

// fkTargetRef renders the schema-qualified, quoted table reference for
// the right-hand side of a REFERENCES clause. Mirrors the splitting
// logic in entitySchema / entityPhysicalTable but operates on the Ref
// directly so callers don't need the target Entity pointer.
func fkTargetRef(r *dsl.Ref) string {
	if r.TargetTableName != "" {
		schema, table := "public", r.TargetTableName
		if i := strings.IndexByte(r.TargetTableName, '.'); i >= 0 {
			schema, table = r.TargetTableName[:i], r.TargetTableName[i+1:]
		}
		return quoteIdent(schema) + "." + quoteIdent(table)
	}
	return `"atlantis".` + quoteIdent(tableNameFromID(r.TargetID))
}

// tableNameFromID converts a canonical "namespace.Entity" into our flat table name.
func tableNameFromID(id string) string {
	parts := strings.SplitN(id, ".", 2)
	if len(parts) != 2 {
		return id
	}
	return parts[0] + "_" + snakeCase(parts[1])
}

// Constraint / index names are deterministic so we can DROP CONSTRAINT and
// DROP INDEX without remembering Postgres's auto-generated names. Each name
// fits within the 63-char Postgres identifier limit (truncation falls back
// to a hash suffix — we keep the unhashed form short by relying on
// snake_case entity names).

func pkName(e *dsl.Entity) string               { return tableName(e) + "_pkey" }
func fkName(e *dsl.Entity, field string) string { return tableName(e) + "_" + field + "_fkey" }
func uqName(e *dsl.Entity, field string) string { return tableName(e) + "_" + field + "_key" }

// compositeUniqueName names a multi-column UNIQUE constraint deterministically.
// The shape is <table>_<field1>_<field2>_..._key so DROP CONSTRAINT can find it.
func compositeUniqueName(e *dsl.Entity, fields []string) string {
	return tableName(e) + "_" + strings.Join(fields, "_") + "_key"
}

func indexName(e *dsl.Entity, idx dsl.Index) string {
	prefix := tableName(e)
	switch idx.Kind {
	case dsl.IndexBtree:
		return prefix + "_" + joinFieldNames(idx.Fields) + "_idx"
	case dsl.IndexPartial:
		if idx.Unique {
			return prefix + "_" + joinFieldNames(idx.Fields) + "_unique_partial_idx"
		}
		return prefix + "_" + joinFieldNames(idx.Fields) + "_partial_idx"
	case dsl.IndexHNSW:
		return prefix + "_" + idx.Field + "_hnsw_idx"
	case dsl.IndexGIN:
		return prefix + "_" + idx.Field + "_gin_idx"
	}
	return prefix + "_idx"
}

// qualifiedIndexName: indexes live in the same schema as the table they
// attach to, so honoring a `table "..."` override here is mandatory —
// otherwise DROP INDEX targets the wrong schema and silently no-ops.
func qualifiedIndexName(e *dsl.Entity, idx dsl.Index) string {
	return quoteIdent(entitySchema(e)) + "." + quoteIdent(indexName(e, idx))
}

func joinFieldNames(fs []dsl.IndexField) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		if f.IsExpr {
			names[i] = exprSlug(f.Expr)
		} else {
			names[i] = f.Name
		}
	}
	return strings.Join(names, "_")
}

// exprSlug turns an arbitrary SQL expression into an identifier-safe slug
// for use in generated index names. We don't try to keep it readable —
// uniqueness and stability are the only requirements. Lowercase alnum,
// everything else compressed to _.
func exprSlug(expr string) string {
	var b strings.Builder
	prev := byte('_')
	for i := range len(expr) {
		c := expr[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			prev = c
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32) // ASCII tolower
			prev = c + 32
		default:
			if prev != '_' {
				b.WriteByte('_')
				prev = '_'
			}
		}
	}
	out := b.String()
	if out == "" {
		return "expr"
	}
	return out
}

func indexFieldList(fs []dsl.IndexField) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		body := quoteIdent(f.Name)
		if f.IsExpr {
			// Wrap the expression in parens — Postgres requires parens
			// around expression-index targets, e.g. CREATE INDEX ... ((lower(email))).
			// Expression bodies are user-supplied SQL fragments; we don't
			// quote them as identifiers (they're not identifiers).
			body = "(" + f.Expr + ")"
		}
		if f.Desc {
			parts[i] = body + " DESC"
		} else {
			parts[i] = body
		}
	}
	return strings.Join(parts, ", ")
}

// joinQuoted renders a list of identifiers as `"a", "b", "c"` — used wherever
// the SQL grammar wants a parenthesized identifier list (composite PK,
// composite UNIQUE).
func joinQuoted(ids []string) string {
	parts := make([]string, len(ids))
	for i, s := range ids {
		parts[i] = quoteIdent(s)
	}
	return strings.Join(parts, ", ")
}

// snakeCase converts UpperCamelCase to snake_case.
// Delegates to the shared schema package.
func snakeCase(s string) string {
	return schema.SnakeCase(s)
}

type sqlBuilder struct{ b strings.Builder }

func (s *sqlBuilder) line(line string) {
	s.b.WriteString(line)
	s.b.WriteByte('\n')
}

func (s *sqlBuilder) linef(format string, args ...any) {
	s.line(fmt.Sprintf(format, args...))
}

func (s *sqlBuilder) blank() {
	s.b.WriteByte('\n')
}

func (s *sqlBuilder) String() string { return s.b.String() }

// EmitInitial generates the initial migration that creates every entity in
// newIR from scratch, in topological order so FK targets exist before they're
// referenced. Used by `tidectl plan` for the very first migration on an empty
// database.
func EmitInitial(newIR *dsl.IR) (SQLScripts, error) {
	if newIR == nil {
		return SQLScripts{}, fmt.Errorf("EmitInitial: newIR is required")
	}
	order, err := topoSortEntities(newIR.Entities)
	if err != nil {
		return SQLScripts{}, err
	}
	up := &sqlBuilder{}
	down := &sqlBuilder{}
	up.line("-- atlantis initial migration")
	up.line("CREATE SCHEMA IF NOT EXISTS atlantis;")
	// Emit CREATE EXTENSION only for what the schema actually needs.
	// A vanilla-text schema shouldn't force operators to install
	// pgvector + timescaledb at the OS level.
	for _, ext := range collectRequiredExtensions(newIR) {
		up.linef("CREATE EXTENSION IF NOT EXISTS %s;", ext)
	}
	// Also create any Postgres schemas that entities with explicit
	// `table "schema.table"` overrides require — `public` always exists
	// and `atlantis` was created above. The down does NOT drop these;
	// a non-atlantis schema may hold unrelated objects.
	for _, name := range collectInitialSchemas(newIR) {
		up.linef("CREATE SCHEMA IF NOT EXISTS %s;", quoteIdent(name))
	}
	up.blank()
	down.line("-- atlantis initial migration (down)")
	for _, e := range order {
		emitEntityCreate(up, e)
		up.blank()
	}
	// Drop in reverse FK order on the down side.
	for i := len(order) - 1; i >= 0; i-- {
		emitEntityDrop(down, order[i])
	}
	down.line("DROP SCHEMA IF EXISTS atlantis CASCADE;")
	return SQLScripts{Up: up.String(), Down: down.String()}, nil
}

// topoSortEntities orders entities so that FK target tables are created
// before the entities that reference them. Cycles (self-references aside)
// are reported as an error.
//
// Self-references are tolerated by emitting the entity but issuing the FK
// constraint AFTER the table — we already use named FK constraints in the
// CREATE TABLE statement, which Postgres accepts even for self-references.
// True cycles between two different tables are an error (we'd need
// to emit the constraint with a separate ALTER TABLE — future work).
func topoSortEntities(entities []dsl.Entity) ([]*dsl.Entity, error) {
	byID := map[string]*dsl.Entity{}
	for i := range entities {
		byID[entities[i].ID()] = &entities[i]
	}
	// Stable input order keyed by ID — entities are already sorted by Lower.
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	color := map[string]int{} // 0=white, 1=gray (on stack), 2=black (done)
	var out []*dsl.Entity
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case 1:
			return fmt.Errorf("FK cycle detected involving %s", id)
		case 2:
			return nil
		}
		color[id] = 1
		e := byID[id]
		for _, f := range e.Fields {
			if f.Ref == nil || f.Ref.TargetID == id {
				continue // skip self-references
			}
			if _, ok := byID[f.Ref.TargetID]; !ok {
				continue // validated elsewhere; skip
			}
			if err := visit(f.Ref.TargetID); err != nil {
				return err
			}
		}
		color[id] = 2
		out = append(out, e)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}
