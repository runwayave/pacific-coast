package sim

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Row holds one row's column values, indexed parallel to TableDesc.Cols.
// We deliberately keep it []any not a typed struct: codegen-emitted SQL
// names columns by string, and the executor binds Go scan targets per
// column at result time, so a positional slice indexed by ColIndex is
// the simplest representation that supports both reads and updates.
//
// Nil entries are NULL. The executor only stores int64 / string / bool /
// time.Time / []byte / []float32; everything else is rejected at bind
// time, so the storage layer stays type-agnostic.
type Row []any

// RowMap is the immutable-once-shared row container the table holds
// via an atomic.Pointer. The shared flag is *advisory*: once a Mark,
// Fork, or restore captures this map, shared transitions to true.
// Subsequent writes detect shared and clone-on-write, swapping in a
// fresh RowMap. That clone is what makes Mark / Fork / RestoreTo
// O(num_tables) instead of O(num_rows).
//
// We never set shared back to false. A map either was never captured
// (mutate in place) or has been captured at some point (clone on
// write). After a clone, the new map starts un-shared until something
// captures it again. The CoW invariant: once a map has been observed
// externally, it must be treated as frozen.
//
// Exported (with unexported fields) so the sandbox package can hold
// stable pointers across the package boundary; only the sim package
// itself reads/writes the internals.
type RowMap struct {
	m      map[string]Row
	shared atomic.Bool
}

// Snapshot returns the row map's contents for read-only inspection.
// Callers (Inspector.Diff, snapshot encoding) MUST NOT mutate the
// returned map — the CoW invariant relies on it being treated as
// immutable from the moment Mark/Fork captured the RowMap.
func (rm *RowMap) Snapshot() map[string]Row { return rm.m }

// Table is the in-memory store for one entity. The PK index lives in
// RowMap.m; reads and writes go through the atomic Pointer so Mark
// and Fork can capture the current map cheaply.
//
// mu serializes writers only — readers consume through Load() and
// never block on the lock. The lock exists because a writer that
// detects shared=true needs to read the current map, build a new
// one, and CAS-swap atomically with respect to other writers; mu
// gives us a writer fence.
type Table struct {
	Desc *TableDesc

	mu         sync.Mutex // writer fence; readers are lock-free via Load
	rows       atomic.Pointer[RowMap]
	nextSerial atomic.Int64
}

// NewTable allocates an empty table for the given descriptor.
func NewTable(d *TableDesc) *Table {
	t := &Table{Desc: d}
	t.rows.Store(&RowMap{m: map[string]Row{}})
	return t
}

// CurrentRows returns the current row-map pointer. Mark / Fork
// capture this; subsequent writes will clone-on-write because we
// also mark the captured map shared.
//
// Callers must NOT mutate the returned map. The Table itself
// enforces this on writes (it clones first if shared); external
// callers honoring the CoW contract see a stable snapshot.
func (t *Table) CurrentRows() *RowMap {
	rm := t.rows.Load()
	rm.shared.Store(true)
	return rm
}

// ReplaceRows installs a previously-captured RowMap as the live one.
// RestoreTo / ImportRows use this to swap state. The incoming map's
// shared flag is preserved — if it was captured by another Mark, it
// stays shared, ensuring subsequent writes still clone.
func (t *Table) ReplaceRows(rm *RowMap) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows.Store(rm)
}

// PKKey builds the canonical key for a row by reading the PK columns
// out of the row in declared order. Used for both insert and lookup.
//
// Encoding: each PK value is stringified via fmt.Sprintf("%v") and
// joined with "\x00". The zero byte can't appear in valid UTF-8
// string columns, and our integer/bool stringifications don't produce
// one either, so distinct PK tuples never collide.
func (t *Table) PKKey(r Row) (string, error) {
	parts := make([]string, len(t.Desc.PKCols))
	for i, pk := range t.Desc.PKCols {
		idx := t.Desc.ColIndex(pk)
		if idx < 0 || idx >= len(r) {
			return "", fmt.Errorf("sandbox: row missing PK column %q", pk)
		}
		v := r[idx]
		if v == nil {
			return "", fmt.Errorf("sandbox: PK column %q is NULL", pk)
		}
		parts[i] = fmt.Sprintf("%v", v)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out = out + "\x00" + p
	}
	return out, nil
}

// writable returns the RowMap to mutate. If the current map has been
// captured (shared=true), it clones first and stores the clone. The
// returned map is exclusively owned by this writer until the unlock.
func (t *Table) writable() *RowMap {
	rm := t.rows.Load()
	if !rm.shared.Load() {
		return rm
	}
	// Clone.
	cloned := &RowMap{m: make(map[string]Row, len(rm.m))}
	for k, v := range rm.m {
		cloned.m[k] = v
	}
	t.rows.Store(cloned)
	return cloned
}

// Insert installs a new row keyed by its PK. Returns the canonical
// key so the executor can re-fetch the inserted row for a RETURNING
// clause without rebuilding the key. Duplicate PK returns ErrPKConflict.
func (t *Table) Insert(r Row) (string, error) {
	key, err := t.PKKey(r)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rm := t.writable()
	if _, exists := rm.m[key]; exists {
		return "", fmt.Errorf("sandbox: %w on %s key=%s",
			ErrPKConflict, t.Desc.Qualified(), key)
	}
	// Copy the row so callers can't mutate stored state by holding the
	// slice header — the writable map is private to this table now,
	// but a caller passing a slice they later mutate would still leak
	// in if we didn't copy.
	cp := make(Row, len(r))
	copy(cp, r)
	rm.m[key] = cp
	return key, nil
}

// Get returns the row by canonical PK key. Returns nil if absent.
// Callers must NOT mutate the returned slice — the CoW contract is
// "treat reads as immutable." Sim's executor honors this by copying
// before any mutation.
func (t *Table) Get(key string) Row {
	return t.rows.Load().m[key]
}

// Update applies a column → new-value patch to the row at key.
// Returns 0 if the key doesn't exist, 1 otherwise — matching the
// CommandTag.RowsAffected() semantics generated code reads. Because
// we treat stored rows as immutable, an in-place mutation would
// corrupt any snapshot that captured this map. So we copy the row
// before patching it.
func (t *Table) Update(key string, set map[string]any) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	rm := t.writable()
	r, ok := rm.m[key]
	if !ok {
		return 0
	}
	// Copy-on-write at the row granularity too: prior snapshots may
	// hold the old Row slice via the cloned map, so we mustn't mutate
	// it in place.
	cp := make(Row, len(r))
	copy(cp, r)
	for col, v := range set {
		idx := t.Desc.ColIndex(col)
		if idx < 0 {
			continue
		}
		cp[idx] = v
	}
	rm.m[key] = cp
	return 1
}

// Delete removes the row at key. Returns 1 if a row was deleted, 0
// otherwise. Same CoW invariants as Insert/Update.
func (t *Table) Delete(key string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	rm := t.writable()
	if _, ok := rm.m[key]; !ok {
		return 0
	}
	delete(rm.m, key)
	return 1
}

// Len returns the row count. Useful for tests; not on the codegen path.
func (t *Table) Len() int {
	return len(t.rows.Load().m)
}

// NextSerial returns the next serial value for IDENTITY/BIGSERIAL
// columns. Existing tests pass explicit IDs; this hook exists for the
// future auto-id-on-insert path.
func (t *Table) NextSerial() int64 { return t.nextSerial.Add(1) }

// Scan invokes fn for every row in deterministic insertion-equivalent
// order (sorted PK keys, which matches what a btree-backed index
// would produce). fn may not mutate the row it receives; copy if you
// need a private working copy.
func (t *Table) Scan(fn func(Row) error) error {
	rm := t.rows.Load()
	keys := make([]string, 0, len(rm.m))
	for k := range rm.m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		row := rm.m[k]
		if row == nil {
			continue
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

// ScanKeyed is Scan that also yields the canonical PK key. Used by
// UPDATE / DELETE executors that want to apply changes via
// Update(key) or Delete(key) without rebuilding the key from the row.
func (t *Table) ScanKeyed(fn func(key string, row Row) error) error {
	rm := t.rows.Load()
	keys := make([]string, 0, len(rm.m))
	for k := range rm.m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		row := rm.m[k]
		if row == nil {
			continue
		}
		if err := fn(k, row); err != nil {
			return err
		}
	}
	return nil
}

// sortStrings sorts in place via insertion sort. Bounded by table
// size and only called from Scan paths; the import-cost saving over
// pulling sort.Strings here is symbolic but the function is one
// loop, so it stays simple.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
