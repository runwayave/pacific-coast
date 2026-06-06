package sandbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// Inspector is the LLM tool-use surface for the sandbox. An agent uses
// Inspect methods to ask the sandbox about its current state —
// "describe this table", "show me sample rows", "find users matching
// X", "diff before-and-after" — without generating any SQL itself.
//
// Inspector lives entirely in-process; the HTTP control plane in
// http.go exposes the same surface at /v1/sandbox/{id}/inspect.
// Method signatures intentionally accept and return Go types (not raw
// protobufs) so they're directly usable from test code and easily
// serialized to JSON by the HTTP layer.
type Inspector struct{ sb *Sandbox }

// Inspect returns the LLM tool-use surface for this sandbox.
func (s *Sandbox) Inspect() *Inspector { return &Inspector{sb: s} }

// ─────────────────────────── Describe ───────────────────────────

// TableDescription is the structured shape Describe returns: schema +
// columns + PK + row count, plus the IR-derived metadata captured
// during catalog construction. Agents read this to understand what an
// entity is *for* before composing queries.
type TableDescription struct {
	Schema             string       `json:"schema"`
	Name               string       `json:"name"`
	Qualified          string       `json:"qualified"`
	Columns            []ColumnInfo `json:"columns"`
	PrimaryKey         []string     `json:"primary_key"`
	RowCount           int          `json:"row_count"`
	SoftDeleteField    string       `json:"soft_delete_field,omitempty"`
	TouchOnUpdateField string       `json:"touch_on_update_field,omitempty"`
	PartitionField     string       `json:"partition_field,omitempty"`
	TimeField          string       `json:"time_field,omitempty"`
	IdentityCol        string       `json:"identity_col,omitempty"`
}

// ColumnInfo describes one column. Kind is the string form (e.g.
// "bigint", "text") rather than the internal ColKind enum so HTTP
// responses are stable across builds.
type ColumnInfo struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Nullable bool   `json:"nullable"`
}

// Describe returns the structured TableDescription for a qualified
// entity name (e.g. "atlantis.consumer_user"). Returns ErrUnknownEntity
// when the catalog has no descriptor by that name.
func (i *Inspector) Describe(qualified string) (*TableDescription, error) {
	desc := i.sb.Catalog().Lookup(qualified)
	if desc == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEntity, qualified)
	}
	cols := make([]ColumnInfo, len(desc.Cols))
	for j, c := range desc.Cols {
		cols[j] = ColumnInfo{Name: c.Name, Kind: kindString(c.Kind), Nullable: c.Nullable}
	}
	// Row count: ExportRows returns per-table slices including empty
	// ones for declared-but-empty tables.
	rowCount := 0
	if rows, ok := i.sb.pool.ExportRows()[qualified]; ok {
		rowCount = len(rows)
	}
	pk := append([]string(nil), desc.PKCols...)
	return &TableDescription{
		Schema:             desc.Schema,
		Name:               desc.Name,
		Qualified:          desc.Qualified(),
		Columns:            cols,
		PrimaryKey:         pk,
		RowCount:           rowCount,
		SoftDeleteField:    desc.SoftDeleteField,
		TouchOnUpdateField: desc.TouchOnUpdateField,
		PartitionField:     desc.PartitionField,
		TimeField:          desc.TimeField,
		IdentityCol:        desc.IdentityCol,
	}, nil
}

// ─────────────────────────── Sample ───────────────────────────

// Sample returns up to n rows from the named table as
// column-name → value maps. Rows are returned in insertion order; the
// sim already sorts its scan iteration deterministically so two
// Sample calls in a stable state return identical results.
//
// n <= 0 returns nil rows. n larger than the row count returns every
// row without error.
func (i *Inspector) Sample(qualified string, n int) ([]map[string]any, error) {
	desc := i.sb.Catalog().Lookup(qualified)
	if desc == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEntity, qualified)
	}
	rows, ok := i.sb.pool.ExportRows()[qualified]
	if !ok {
		return nil, nil
	}
	if n <= 0 {
		return nil, nil
	}
	if n > len(rows) {
		n = len(rows)
	}
	out := make([]map[string]any, n)
	for k := 0; k < n; k++ {
		out[k] = rowToMap(desc, rows[k])
	}
	return out, nil
}

// ─────────────────────────── Find ───────────────────────────

// Predicate is the closed-grammar filter shape Find accepts: a column,
// a comparison op, and a value. Agents construct one of these per AND
// clause; an LLM front-end can lower natural-language filters into
// this shape without re-architecting Inspect.
type Predicate struct {
	Column string
	Op     PredicateOp
	Value  any
}

// PredicateOp enumerates the comparison operators Find recognizes.
// Mirrors sim/sql's CmpOp but exists separately so the public API
// doesn't pull internal sub-packages.
type PredicateOp string

const (
	PredEq      PredicateOp = "="
	PredNE      PredicateOp = "!="
	PredLT      PredicateOp = "<"
	PredLE      PredicateOp = "<="
	PredGT      PredicateOp = ">"
	PredGE      PredicateOp = ">="
	PredIsNull  PredicateOp = "is null"
	PredNotNull PredicateOp = "is not null"
)

// Find returns every row matching the supplied predicates (implicit
// AND). Returns an empty slice when no row matches; error only on
// unknown column or unknown entity. Pure read-only — Find never
// mutates state.
func (i *Inspector) Find(qualified string, preds ...Predicate) ([]map[string]any, error) {
	desc := i.sb.Catalog().Lookup(qualified)
	if desc == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEntity, qualified)
	}
	for _, p := range preds {
		if desc.ColIndex(p.Column) < 0 {
			return nil, fmt.Errorf("%w: %s on %s", ErrUnknownColumn, p.Column, qualified)
		}
	}
	rows, ok := i.sb.pool.ExportRows()[qualified]
	if !ok {
		return nil, nil
	}
	var out []map[string]any
	for _, r := range rows {
		match, err := rowMatchesPredicates(desc, r, preds)
		if err != nil {
			return nil, err
		}
		if match {
			out = append(out, rowToMap(desc, r))
		}
	}
	return out, nil
}

// ─────────────────────────── Diff ───────────────────────────

// DiffResult is what Diff returns: per-table counts of how many rows
// were added, removed, or modified between two Marks. An agent loop
// inspecting "what did my last attempt change?" calls Diff(beforeMark,
// nowMark) and reads this directly.
type DiffResult struct {
	Tables map[string]TableDiff `json:"tables"`
}

// TableDiff is one table's add/remove/modify count. Modified counts
// rows with the same PK that differ in non-PK columns; rows whose PK
// changes are counted as one Removed + one Added.
type TableDiff struct {
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	Modified int `json:"modified"`
}

// Diff compares two marks of the same sandbox. ErrMarkOwnerMismatch
// when either mark wasn't produced by this sandbox.
func (i *Inspector) Diff(a, b *Mark) (*DiffResult, error) {
	if a == nil || b == nil {
		return nil, errors.New("sandbox inspect: nil mark")
	}
	if a.owner != i.sb || b.owner != i.sb {
		return nil, ErrMarkOwnerMismatch
	}
	res := &DiffResult{Tables: map[string]TableDiff{}}
	// Union of qualified table names so newly-created (or removed)
	// tables are accounted for.
	names := map[string]struct{}{}
	for n := range a.captured {
		names[n] = struct{}{}
	}
	for n := range b.captured {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, qn := range sorted {
		desc := i.sb.Catalog().Lookup(qn)
		if desc == nil {
			// Catalog dropped the table since the marks were taken.
			res.Tables[qn] = TableDiff{Removed: snapshotLen(a.captured[qn])}
			continue
		}
		td := diffMarkPair(desc, a.captured[qn], b.captured[qn])
		if td.Added != 0 || td.Removed != 0 || td.Modified != 0 {
			res.Tables[qn] = td
		}
	}
	return res, nil
}

// snapshotLen returns the row count of a captured RowMap or 0 if nil.
func snapshotLen(rm *sim.RowMap) int {
	if rm == nil {
		return 0
	}
	return len(rm.Snapshot())
}

// diffMarkPair counts add/remove/modify between two CoW snapshots of
// the same table. Same logic as the old map-based diffTables but
// reads through sim.RowMap.Snapshot() rather than a copied slice.
func diffMarkPair(desc *sim.TableDesc, before, after *sim.RowMap) TableDiff {
	var beforeMap, afterMap map[string]sim.Row
	if before != nil {
		beforeMap = before.Snapshot()
	}
	if after != nil {
		afterMap = after.Snapshot()
	}
	var td TableDiff
	for k, brow := range beforeMap {
		arow, ok := afterMap[k]
		if !ok {
			td.Removed++
			continue
		}
		if !rowsEqual(brow, arow) {
			td.Modified++
		}
	}
	for k := range afterMap {
		if _, ok := beforeMap[k]; !ok {
			td.Added++
		}
	}
	_ = desc // descriptor reserved for future per-column normalisation
	return td
}

// ─────────────────────────── helpers ───────────────────────────

// rowToMap converts the positional Row to a name-keyed map for the
// agent-facing output shape.
func rowToMap(desc *sim.TableDesc, r sim.Row) map[string]any {
	out := make(map[string]any, len(desc.Cols))
	for i, c := range desc.Cols {
		out[c.Name] = r[i]
	}
	return out
}

// rowMatchesPredicates is Find's per-row evaluator. AND of every
// predicate; NULL on either side of a comparison falls through to
// false (matches PG's three-valued logic).
func rowMatchesPredicates(desc *sim.TableDesc, r sim.Row, preds []Predicate) (bool, error) {
	for _, p := range preds {
		idx := desc.ColIndex(p.Column)
		val := r[idx]
		switch p.Op {
		case PredIsNull:
			if val != nil {
				return false, nil
			}
		case PredNotNull:
			if val == nil {
				return false, nil
			}
		case PredEq:
			if !valueEquals(val, p.Value) {
				return false, nil
			}
		case PredNE:
			if valueEquals(val, p.Value) {
				return false, nil
			}
		case PredLT, PredLE, PredGT, PredGE:
			if val == nil || p.Value == nil {
				return false, nil
			}
			c := compareForInspect(val, p.Value)
			ok := false
			switch p.Op {
			case PredLT:
				ok = c < 0
			case PredLE:
				ok = c <= 0
			case PredGT:
				ok = c > 0
			case PredGE:
				ok = c >= 0
			}
			if !ok {
				return false, nil
			}
		default:
			return false, fmt.Errorf("sandbox inspect: unknown op %q", p.Op)
		}
	}
	return true, nil
}

// valueEquals reuses the sim's NULL-as-unknown semantics: nil never
// equals anything, including another nil.
func valueEquals(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	return a == b
}

// compareForInspect handles the scalar types Inspect sees through
// row-map values. We don't import sim's internal compareValues to
// keep the public API surface independent.
func compareForInspect(a, b any) int {
	switch av := a.(type) {
	case int64:
		if bv, ok := b.(int64); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			switch {
			case !av && bv:
				return -1
			case av && !bv:
				return 1
			}
			return 0
		}
	}
	return 0
}

// diffTables computes a TableDiff between two row slices by PK.
// PK is reconstructed from desc.PKCols + row positions; rows whose
// PK key matches but values differ count as Modified.
func diffTables(desc *sim.TableDesc, before, after []sim.Row) TableDiff {
	beforeByKey := indexByPK(desc, before)
	afterByKey := indexByPK(desc, after)
	var td TableDiff
	for k, brow := range beforeByKey {
		arow, ok := afterByKey[k]
		if !ok {
			td.Removed++
			continue
		}
		if !rowsEqual(brow, arow) {
			td.Modified++
		}
	}
	for k := range afterByKey {
		if _, ok := beforeByKey[k]; !ok {
			td.Added++
		}
	}
	return td
}

func indexByPK(desc *sim.TableDesc, rows []sim.Row) map[string]sim.Row {
	out := make(map[string]sim.Row, len(rows))
	for _, r := range rows {
		parts := make([]string, len(desc.PKCols))
		for i, pk := range desc.PKCols {
			parts[i] = fmt.Sprintf("%v", r[desc.ColIndex(pk)])
		}
		key := strings.Join(parts, "\x00")
		out[key] = r
	}
	return out
}

func rowsEqual(a, b sim.Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valueEquals(a[i], b[i]) {
			// valueEquals returns false on nil; we want nil==nil to
			// count as equal for diff purposes since both are absent.
			if a[i] == nil && b[i] == nil {
				continue
			}
			return false
		}
	}
	return true
}

// kindString renders sim.ColKind into the public string form. Kept
// in this file (rather than on sim.ColKind) because the kind names
// here are the *public* API names — the internal enum can renumber
// without breaking HTTP clients.
func kindString(k sim.ColKind) string {
	switch k {
	case sim.KindInt64:
		return "int64"
	case sim.KindString:
		return "string"
	case sim.KindBool:
		return "bool"
	case sim.KindTime:
		return "timestamptz"
	case sim.KindBytes:
		return "bytes"
	case sim.KindNumeric:
		return "numeric"
	case sim.KindVector:
		return "vector"
	}
	return "unknown"
}
