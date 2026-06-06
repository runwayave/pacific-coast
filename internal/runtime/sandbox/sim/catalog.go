package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ColKind is the scalar type the simulator stores per column. The set is
// closed: every supported PG-side type maps onto exactly one ColKind,
// and the executor switches on this fixed enum when binding placeholders
// or scanning results. New PG types land here, not as a freeform string
// tag, so the fidelity matrix stays honest.
type ColKind uint8

const (
	KindUnknown ColKind = iota
	KindInt64           // smallint / int / bigint / identity / serial
	KindString          // text / varchar / citext / uuid (string-shaped)
	KindBool
	KindTime    // timestamptz / date — stored as time.Time
	KindBytes   // bytea / jsonb (opaque) — stored as []byte
	KindNumeric // numeric(P,S) — stored as string to preserve precision
	KindVector  // pgvector vector(N) — stored as []float32; supports <=>, <->, <#> distance ops in projection / ORDER BY / WHERE.
	// KindArray stores PG array columns (text[], int[], etc.) as the
	// caller's bound Go slice (typically []string / []int64). Sim
	// supports basic INSERT/SELECT round-trip; PG array operators
	// (= ANY(col), @>, &&, unnest) stay unsupported and error at
	// query time. Real schemas use array columns heavily (e.g.
	// VibeMessage.outfit_keys), so storing them opaquely keeps the
	// sandbox bootable on real IRs.
	KindArray
)

// Column describes one table column. Nullability gates whether scan dst
// must be a *sql.NullX shape — generated handlers use sql.NullX for every nullable column, matching the pg adapter.
// for every nullable column the same way the pg adapter does.
type Column struct {
	Name     string
	Kind     ColKind
	Nullable bool
}

// TableDesc is the schema for one entity. PKCols is the ordered list of
// primary key columns; the IR-driven builder fills the metadata fields
// (SoftDeleteField, TouchOnUpdateField, etc.) from the *dsl.Entity so
// the executor + future "auto-apply soft-delete filter" passes can
// honor declared semantics without re-walking the IR.
//
// The metadata fields are also part of the snapshot schema signature
// (see sandbox.Snapshot) so a snapshot taken under an entity with
// `partition by tenant` cannot be loaded back into a catalog that has
// dropped the partition — the sandbox refuses the restore rather than
// silently revealing cross-tenant rows.
type TableDesc struct {
	Schema string
	Name   string
	Cols   []Column
	PKCols []string

	// SoftDeleteField is the timestamptz column whose non-NULL value
	// means the row is soft-deleted. Empty for hard-delete entities.
	SoftDeleteField string

	// TouchOnUpdateField is the timestamptz column refreshed on every
	// UPDATE. Empty when the entity has no touch_on_update modifier.
	TouchOnUpdateField string

	// PartitionField is the column generated QueryX handlers AND-inject
	// for multi-tenant isolation. Empty when no partition is declared.
	PartitionField string

	// TimeField is set for hypertable entities (sim treats hypertable as
	// regular table; the field is captured for snapshot fidelity and
	// for future "warn-once on production performance" hooks).
	TimeField string

	// IdentityCol names the column generated codegen treats as IDENTITY
	// or BIGSERIAL. The sim's atomic counter on Table is intended to feed
	// auto-id-on-insert when it ships; the IR translator currently only records the column name.
	IdentityCol string
}

// Qualified mirrors sql.TableRef.Qualified — same shape so the executor
// can use it as a catalog key without splitting/joining repeatedly.
func (t *TableDesc) Qualified() string { return t.Schema + "." + t.Name }

// ColIndex returns the column index by name. Returns -1 if missing. The
// executor calls this for every column reference because column lookups
// happen per-statement, not per-row; allocating a map per descriptor
// would be more work than O(N) for the ~10-20 columns most entities have.
func (t *TableDesc) ColIndex(name string) int {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return i
		}
	}
	return -1
}

// IsPKCol reports whether name appears in PKCols. Used by the executor
// to enforce the "UPDATE SET never includes PK columns" invariant the
// codegen output respects.
func (t *TableDesc) IsPKCol(name string) bool {
	for _, p := range t.PKCols {
		if p == name {
			return true
		}
	}
	return false
}

// Catalog is the schema registry the executor reads. Tables are keyed by
// their Qualified() string so the executor can look one up by the parsed
// AST's TableRef without unpacking it. The catalog is read-mostly: built
// once at sandbox boot, then accessed concurrently by every transaction.
type Catalog struct {
	mu     sync.RWMutex
	tables map[string]*TableDesc
}

// NewCatalog returns an empty catalog. Most callers boot one via
// sandbox.LoadIR (see internal/runtime/sandbox/ir.go); direct
// RegisterTable calls are reserved for test code.
func NewCatalog() *Catalog {
	return &Catalog{tables: map[string]*TableDesc{}}
}

// RegisterTable installs a table descriptor. Returns an error if a table
// by the same qualified name is already registered — we treat schema as
// build-time immutable, so re-registration is a programmer error.
func (c *Catalog) RegisterTable(d *TableDesc) error {
	if d.Schema == "" || d.Name == "" {
		return fmt.Errorf("sandbox catalog: table needs Schema and Name")
	}
	if len(d.PKCols) == 0 {
		return fmt.Errorf("sandbox catalog: table %s has no PK", d.Qualified())
	}
	for _, p := range d.PKCols {
		if d.ColIndex(p) < 0 {
			return fmt.Errorf("sandbox catalog: %s PK column %q not declared",
				d.Qualified(), p)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[d.Qualified()]; ok {
		return fmt.Errorf("sandbox catalog: table %s already registered", d.Qualified())
	}
	c.tables[d.Qualified()] = d
	return nil
}

// Lookup returns the descriptor for a qualified name or nil.
func (c *Catalog) Lookup(qualified string) *TableDesc {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tables[qualified]
}

// QualifiedNames returns the sorted list of table qualified names.
// Snapshot encoding + restore walk the catalog in this order so the
// blob is deterministic regardless of map iteration order.
func (c *Catalog) QualifiedNames() []string {
	c.mu.RLock()
	out := make([]string, 0, len(c.tables))
	for k := range c.tables {
		out = append(out, k)
	}
	c.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Signature returns a hex-encoded SHA-256 over the canonical
// representation of every registered TableDesc. Snapshot writes the
// signature alongside the rows; Restore refuses to load when the
// current catalog's signature doesn't match. The signature covers
// schema + columns (name, kind, nullability) + PK + the IR-derived
// metadata (soft-delete, touch_on_update, partition, time-field,
// identity) so a schema change in any of those refuses to silently
// load incompatible rows.
//
// Canonical form: one line per table, format
//
//	"<schema>.<name>|cols=<col1:<kind>:<nullable>|...|pk=<pk1>,<pk2>...|sd=<col>|tu=<col>|p=<col>|tf=<col>|id=<col>"
//
// sorted by qualified name. Any change to this format is a snapshot
// format bump — existing snapshot blobs will refuse to load until
// re-snapshotted, which is the safer-by-default behavior.
func (c *Catalog) Signature() string {
	names := c.QualifiedNames()
	var b strings.Builder
	for _, n := range names {
		c.mu.RLock()
		d := c.tables[n]
		c.mu.RUnlock()
		if d == nil {
			continue
		}
		b.WriteString(d.canonical())
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// canonical renders one TableDesc to the deterministic string the
// signature hashes. Kept on the descriptor so test code can read what
// changes when a snapshot mismatch shows up.
func (d *TableDesc) canonical() string {
	var b strings.Builder
	b.WriteString(d.Qualified())
	b.WriteString("|cols=")
	for i, c := range d.Cols {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s:%d:%v", c.Name, c.Kind, c.Nullable)
	}
	b.WriteString("|pk=")
	b.WriteString(strings.Join(d.PKCols, ","))
	fmt.Fprintf(&b, "|sd=%s|tu=%s|p=%s|tf=%s|id=%s",
		d.SoftDeleteField, d.TouchOnUpdateField,
		d.PartitionField, d.TimeField, d.IdentityCol)
	return b.String()
}
