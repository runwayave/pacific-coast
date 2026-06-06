// Package sim is the pure-Go simulator backend for the atlantis sandbox.
// Pool implements runtime.Pool and Tx implements runtime.Tx; together
// they substitute for internal/storage/pg.Pool so the generated server
// can run unchanged against an in-memory store.
//
// Pool implements runtime.Pool over an in-memory map of tables. The
// map is CoW at the RowMap level (see table.go) so Mark and Fork
// capture O(num_tables) pointers without copying rows. Tx is currently
// a thin passthrough — Commit and Rollback are no-ops; rollback-rewinds
// are not implemented and generated handlers don't depend on them for
// read correctness.
//
// SQL parsing routes through sim/sql (pg_query_go translation + the
// executor's accepted-shape whitelist). Anything outside the whitelist
// returns ErrUnsupported.
//
// Clock injection lets sandbox.Options.Clock drive now(); the
// StrictDeterministic mode uses the same hook to make runs
// bit-for-bit reproducible.
//
// The package owns no exported state besides the constructors, the Pool
// type, the Options struct, and the error sentinels in errors.go.
// Catalog and Table are exported so test code can pre-seed schemas;
// the IR-driven boot path is sandbox.LoadIR in the parent package.
package sim

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
	simsql "github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim/sql"
)

// PoolOptions tunes a sim.Pool. Empty value yields working defaults
// (real wall-clock, log-package warn sink, no extra plumbing). Future
// fields land here as future features come online.
type PoolOptions struct {
	// Clock returns "now" for the simulator. nil → time.Now.
	Clock func() time.Time

	// Warn receives one-shot fidelity warnings emitted by the sim —
	// most prominently the "hypertable executed as regular table"
	// notice the fidelity matrix promises. nil → log.Printf with
	// "sandbox warn: " prefix. Tests inject a slice-collector so
	// assertions can read the emitted lines without parsing log
	// output.
	Warn func(string)
}

// withDefaults fills in zero-value defaults. The defaults preserve
// existing behavior — code paths that didn't set these fields keep
// working unchanged.
func (o PoolOptions) withDefaults() PoolOptions {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Warn == nil {
		o.Warn = defaultWarn
	}
	return o
}

func defaultWarn(msg string) { log.Printf("sandbox warn: %s", msg) }

// Pool is the runtime.Pool implementation backed by in-memory tables.
// The catalog is read-mostly; the tables map is mutated under per-table
// locks (held by Table itself), so Pool does not need a top-level mutex
// for the steady-state execution path. Concurrent BeginTx is safe.
//
// hyperWarned guards the once-per-table fidelity warning that fires
// the first time a hypertable is referenced via SQL. Tracking on the
// pool (not the table) keeps the table struct lean.
type Pool struct {
	catalog       *Catalog
	tables        map[string]*Table
	opts          PoolOptions
	hyperWarnedMu sync.Mutex
	hyperWarned   map[string]struct{}
}

// NewPool returns a Pool wired to the given catalog. Table storage is
// allocated lazily as statements reference each table — matches the
// behavior of an empty Postgres database where CREATE TABLE allocates
// nothing until data arrives.
func NewPool(catalog *Catalog) *Pool {
	return NewPoolWithOptions(catalog, PoolOptions{})
}

// NewPoolWithOptions is NewPool plus a PoolOptions override. The sandbox
// façade uses this to forward sandbox.Options.Clock into the simulator.
func NewPoolWithOptions(catalog *Catalog, opts PoolOptions) *Pool {
	return &Pool{
		catalog:     catalog,
		tables:      map[string]*Table{},
		opts:        opts.withDefaults(),
		hyperWarned: map[string]struct{}{},
	}
}

// warnHypertableOnce emits the fidelity-matrix warning the first
// time `qualified` is referenced. Idempotent across concurrent
// callers (one fire per qualified name).
func (p *Pool) warnHypertableOnce(qualified string) {
	desc := p.catalog.Lookup(qualified)
	if desc == nil || desc.TimeField == "" {
		return
	}
	p.hyperWarnedMu.Lock()
	defer p.hyperWarnedMu.Unlock()
	if _, ok := p.hyperWarned[qualified]; ok {
		return
	}
	p.hyperWarned[qualified] = struct{}{}
	p.opts.Warn(fmt.Sprintf(
		"hypertable %s executed as regular table (time_field=%s); production performance not represented",
		qualified, desc.TimeField))
}

// Catalog exposes the underlying catalog so tests and the IR-driven
// boot helper can register descriptors after construction.
func (p *Pool) Catalog() *Catalog { return p.catalog }

// ExportRows dumps every table's rows by qualified name. Used by
// the cross-process Snapshot path (which gob-encodes the result) and
// historically by the in-process Mark path; the in-process Mark now
// uses CapturePointers for O(num_tables) capture.
//
// Returned rows are copies of the underlying storage so the caller
// can mutate them without affecting live state.
//
// Tables registered with the catalog but never written to are
// returned with an empty (non-nil) slice so the snapshot round-trip
// preserves "table exists, no rows" vs "table was unknown" distinctly.
func (p *Pool) ExportRows() map[string][]Row {
	out := map[string][]Row{}
	for _, qn := range p.catalog.QualifiedNames() {
		var rows []Row
		if t, ok := p.tables[qn]; ok {
			_ = t.Scan(func(r Row) error {
				cp := make(Row, len(r))
				copy(cp, r)
				rows = append(rows, cp)
				return nil
			})
		}
		if rows == nil {
			rows = []Row{}
		}
		out[qn] = rows
	}
	return out
}

// CapturePointers returns the current RowMap pointer for every
// registered table, marking each one shared so subsequent writes
// clone-on-write. This is the O(num_tables) capture path Mark uses
// — no row data is copied. Combined with ReplaceFromPointers it
// gives Mark/RestoreTo their published O(num_tables) budget.
func (p *Pool) CapturePointers() map[string]*RowMap {
	out := make(map[string]*RowMap, len(p.tables))
	for _, qn := range p.catalog.QualifiedNames() {
		if t, ok := p.tables[qn]; ok {
			out[qn] = t.CurrentRows()
		}
	}
	return out
}

// ReplaceFromPointers swaps each table's live RowMap with the one in
// the captured snapshot. RestoreTo uses this; cost is O(num_tables),
// independent of row count.
func (p *Pool) ReplaceFromPointers(snap map[string]*RowMap) {
	for qn, rm := range snap {
		if t, ok := p.tables[qn]; ok {
			t.ReplaceRows(rm)
		}
	}
	// Tables present in the catalog but missing from the snapshot are
	// reset to an empty RowMap — a snapshot represents a complete
	// state, so anything not in it must be cleared.
	for _, qn := range p.catalog.QualifiedNames() {
		if _, ok := snap[qn]; ok {
			continue
		}
		if t, ok := p.tables[qn]; ok {
			t.ReplaceRows(&RowMap{m: map[string]Row{}})
		}
	}
}

// ForkPointers returns the per-table RowMap pointers a fork should
// share with this pool. Identical to CapturePointers semantically —
// both paths need to mark the live maps as shared so subsequent
// writes on either side clone-on-write. Exposed separately so the
// sandbox fork helper reads as a fork, not a mark.
func (p *Pool) ForkPointers() map[string]*RowMap { return p.CapturePointers() }

// InstallSharedPointers populates this pool's tables from a captured
// snapshot. Used by Fork on the child sandbox: the child pool starts
// life with RowMaps pointer-shared with the parent; first write on
// either side clones. Tables in the catalog but not in the snapshot
// get fresh empty RowMaps.
func (p *Pool) InstallSharedPointers(snap map[string]*RowMap) {
	for _, qn := range p.catalog.QualifiedNames() {
		desc := p.catalog.Lookup(qn)
		t := NewTable(desc)
		if rm, ok := snap[qn]; ok {
			t.ReplaceRows(rm)
		}
		p.tables[qn] = t
	}
}

// ImportRows resets every table to the supplied row set. Tables present
// in the import but not in the catalog are rejected — that's the
// schema-mismatch signal the snapshot loader uses. Tables in the
// catalog but missing from the import are cleared (treated as empty),
// matching the "snapshot represents a complete state" contract.
//
// This is a destructive operation; the caller (sandbox.Restore) is
// expected to have already verified the catalog signature matches.
func (p *Pool) ImportRows(rows map[string][]Row) error {
	for qn := range rows {
		if p.catalog.Lookup(qn) == nil {
			return fmt.Errorf("sandbox import: table %s not in catalog", qn)
		}
	}
	for _, qn := range p.catalog.QualifiedNames() {
		desc := p.catalog.Lookup(qn)
		tbl := NewTable(desc)
		for _, r := range rows[qn] {
			cp := make(Row, len(r))
			copy(cp, r)
			if _, err := tbl.Insert(cp); err != nil {
				return fmt.Errorf("sandbox import: %s: %w", qn, err)
			}
		}
		p.tables[qn] = tbl
	}
	return nil
}

// Compile-time interface check; if the generated server's runtime.Pool
// contract changes shape, this line lights up.
var _ runtime.Pool = (*Pool)(nil)

// QueryRow runs a query expected to return at most one row. Errors
// during parse or execution surface in Row.Scan so callers can use
// runtime.IsNoRows + the error-bubble pattern the pg adapter established.
func (p *Pool) QueryRow(_ context.Context, sql string, args ...any) runtime.Row {
	stmt, err := simsql.Parse(sql)
	if err != nil {
		return errRow{err: err}
	}
	env := p.env(args)

	switch s := stmt.(type) {
	case *simsql.Select:
		rows, _, desc, err := execSelect(s, env)
		if err != nil {
			return errRow{err: err}
		}
		if len(rows) == 0 {
			return errRow{err: ErrNoRows}
		}
		return &simRow{projs: s.Cols, row: rows[0], desc: desc}
	case *simsql.Insert:
		// INSERT ... RETURNING ... is routed through QueryRow by the pg
		// adapter; we mirror that. The returned row is the freshly
		// inserted one; scan reads from it using the RETURNING column
		// names.
		if len(s.Returning) == 0 {
			return errRow{err: fmt.Errorf("sandbox: QueryRow over INSERT requires RETURNING")}
		}
		_, row, desc, err := execInsert(s, env)
		if err != nil {
			return errRow{err: err}
		}
		return &returningRow{cols: s.Returning, row: row, desc: desc}
	}
	return errRow{err: fmt.Errorf("%w: %T not valid for QueryRow", simsql.ErrUnsupported, stmt)}
}

// Query runs a multi-row statement. The whitelist covers SELECT with WHERE /
// ORDER BY / LIMIT / OFFSET / window COUNT, plus INSERT ... RETURNING.
func (p *Pool) Query(_ context.Context, sql string, args ...any) (runtime.Rows, error) {
	stmt, err := simsql.Parse(sql)
	if err != nil {
		return nil, err
	}
	env := p.env(args)

	switch s := stmt.(type) {
	case *simsql.Select:
		rows, _, desc, err := execSelect(s, env)
		if err != nil {
			return nil, err
		}
		return &simRows{projs: s.Cols, desc: desc, rows: rows}, nil
	case *simsql.Insert:
		if len(s.Returning) == 0 {
			return nil, fmt.Errorf("sandbox: Query over INSERT requires RETURNING")
		}
		_, row, desc, err := execInsert(s, env)
		if err != nil {
			return nil, err
		}
		return &returningRows{cols: s.Returning, desc: desc, rows: []Row{row}}, nil
	}
	return nil, fmt.Errorf("%w: %T not valid for Query", simsql.ErrUnsupported, stmt)
}

// Exec runs a non-returning statement. Returns a CommandTag whose
// RowsAffected() reports the affected count; UPDATE / DELETE / non-
// returning INSERT all go through this path.
func (p *Pool) Exec(_ context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	stmt, err := simsql.Parse(sql)
	if err != nil {
		return nil, err
	}
	env := p.env(args)

	switch s := stmt.(type) {
	case *simsql.Insert:
		// Tolerate an unused RETURNING — the caller chose Exec so they
		// don't care about the returned columns. We still execute and
		// report rowsAffected=1.
		n, _, _, err := execInsert(s, env)
		if err != nil {
			return nil, err
		}
		return cmdTag(n), nil
	case *simsql.Update:
		n, err := execUpdate(s, env)
		if err != nil {
			return nil, err
		}
		return cmdTag(n), nil
	case *simsql.Delete:
		n, err := execDelete(s, env)
		if err != nil {
			return nil, err
		}
		return cmdTag(n), nil
	}
	return nil, fmt.Errorf("%w: %T not valid for Exec", simsql.ErrUnsupported, stmt)
}

// BeginTx returns a transaction handle that shares the pool's table
// map. Commit and Rollback are no-ops — there is no per-tx snapshot,
// so writes are visible immediately and rollback does not rewind them.
func (p *Pool) BeginTx(_ context.Context) (runtime.Tx, error) {
	return &simTx{pool: p}, nil
}

// env builds the per-statement execution environment. The args slice is
// prefixed with a nil so a $1 placeholder reads args[1] directly. The
// pool reference threads through so per-table fidelity warnings (e.g.
// hypertable-as-regular-table) can fire from inside the executor.
func (p *Pool) env(args []any) *execEnv {
	bound := make([]any, len(args)+1)
	copy(bound[1:], args)
	return &execEnv{
		catalog: p.catalog,
		tables:  p.tables,
		args:    bound,
		clock:   p.opts.Clock,
		pool:    p,
	}
}

// errRow lets QueryRow defer all parse and exec errors to Scan, mirroring
// the pgx adapter's "errors land at Scan time" contract. Generated code
// reads the error here and checks runtime.IsNoRows to translate the
// no-rows case into ErrNotFound at the RPC layer.
type errRow struct{ err error }

func (e errRow) Scan(_ ...any) error { return e.err }

// simRow is a single-row result for QueryRow over a SELECT. It carries
// the projections so the scan layer dispatches window-count vs column
// types correctly.
type simRow struct {
	projs []simsql.Projection
	row   []any
	desc  *TableDesc
}

func (r *simRow) Scan(dest ...any) error { return scanProjected(r.desc, r.projs, r.row, dest) }

// returningRow is a single-row result for INSERT ... RETURNING. The
// RETURNING list is `[]string` (column names) so we reuse the simpler
// scanInto path.
type returningRow struct {
	cols []string
	row  Row
	desc *TableDesc
}

func (r *returningRow) Scan(dest ...any) error { return scanInto(r.desc, r.row, r.cols, dest) }

// simRows is the forward-only cursor over SELECT results.
type simRows struct {
	projs []simsql.Projection
	desc  *TableDesc
	rows  [][]any
	idx   int
	cur   []any
	err   error
}

func (r *simRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.cur = r.rows[r.idx]
	r.idx++
	return true
}

func (r *simRows) Scan(dest ...any) error {
	if r.cur == nil {
		return errors.New("sandbox rows: Scan called before Next")
	}
	if err := scanProjected(r.desc, r.projs, r.cur, dest); err != nil {
		r.err = err
		return err
	}
	return nil
}

func (r *simRows) Err() error { return r.err }
func (r *simRows) Close()     {}

// Columns returns the projection-aligned column names for the result
// set. The HTTP layer uses this to label the JSON response columns
// without having to re-scan the SQL — important for `SELECT *` where
// a string scanner can't know the post-expansion column names.
//
//   - Bare column projection → the column name
//   - Window count           → the alias (`COUNT(*) OVER () AS total` → "total")
//   - Expression with alias  → the alias (vector distance, JSON extract)
//   - Bare expression        → "?column?" (PG's default name for unaliased exprs)
func (r *simRows) Columns() []string {
	return projectionColumnNames(r.projs)
}

// projectionColumnNames is shared between simRows.Columns and any
// future caller (e.g. an Inspect-shaped JSON response builder).
func projectionColumnNames(projs []simsql.Projection) []string {
	out := make([]string, len(projs))
	for i, p := range projs {
		switch {
		case p.IsWindowCount():
			out[i] = p.WindowCountAlias
		case p.Alias != "":
			out[i] = p.Alias
		case p.Column != "":
			out[i] = p.Column
		default:
			out[i] = "?column?"
		}
	}
	return out
}

// returningRows is the multi-row variant for INSERT ... RETURNING used
// through Query (codegen sometimes routes RETURNING through Query when
// the response shape is a list; the contract is the same).
type returningRows struct {
	cols []string
	desc *TableDesc
	rows []Row
	idx  int
	cur  Row
	err  error
}

func (r *returningRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.cur = r.rows[r.idx]
	r.idx++
	return true
}

func (r *returningRows) Scan(dest ...any) error {
	if r.cur == nil {
		return errors.New("sandbox rows: Scan called before Next")
	}
	if err := scanInto(r.desc, r.cur, r.cols, dest); err != nil {
		r.err = err
		return err
	}
	return nil
}

func (r *returningRows) Err() error { return r.err }
func (r *returningRows) Close()     {}

// Columns mirrors simRows.Columns for the RETURNING path — the cols
// slice was already provided by the caller (INSERT RETURNING a, b).
func (r *returningRows) Columns() []string {
	out := make([]string, len(r.cols))
	copy(out, r.cols)
	return out
}

// cmdTag is the trivial CommandTag implementation — the integer count
// is what every codegen-emitted UPDATE/DELETE path reads.
type cmdTag int64

func (c cmdTag) RowsAffected() int64 { return int64(c) }

// simTx routes every method directly to the pool's storage; no per-tx
// isolation. The interface shape is preserved so codegen-emitted
// BeginTx/Exec/Commit sequences run unchanged.
type simTx struct{ pool *Pool }

func (t *simTx) QueryRow(ctx context.Context, sql string, args ...any) runtime.Row {
	return t.pool.QueryRow(ctx, sql, args...)
}
func (t *simTx) Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error) {
	return t.pool.Query(ctx, sql, args...)
}
func (t *simTx) Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error) {
	return t.pool.Exec(ctx, sql, args...)
}
func (t *simTx) Commit(_ context.Context) error   { return nil }
func (t *simTx) Rollback(_ context.Context) error { return nil }
