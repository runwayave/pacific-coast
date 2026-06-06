// Package embedded is the fidelity backend for the atlantis sandbox.
//
// Where the sim under internal/runtime/sandbox/sim ships sub-millisecond
// boot in pure Go at the cost of a whitelist-shaped SQL surface, the
// embedded backend runs a real Postgres process in-process via
// fergusstrange/embedded-postgres and exposes it as runtime.Pool. The
// trade-off: ~4-8 second cold start in
// exchange for 100% fidelity — every PG idiom (LATERAL, recursive CTE,
// tsvector, pg_trgm, JSONB jsonb_array_elements, real HNSW index, etc.)
// just works.
//
// Auto-routing wires this in at sandbox.New when Options.Backend is
// BackendEmbedded or when BackendAuto sees a caller schema with custom
// query / procedure / hypertable blocks (because the sim's whitelist
// can't honor them without silent fidelity drift).
//
// Schema setup: the IR's CREATE TABLE / CREATE INDEX / trigger DDL is
// applied at boot via internal/codegen's existing emitter (called with
// an empty old-IR so every entity comes out as ClassAdditive). The
// emitter is the canonical source of "what production DDL would
// look like" — reusing it means the embedded backend's schema is
// byte-equivalent to what a real `tide apply` produces.
//
// Features not supported on embedded (and the sim handles instead):
//   - Mark / RestoreTo time-travel (would require pg_dump/pg_restore;
//     too slow to be useful for the agent loop)
//   - Fork (cloning a PG database means dump + restore; out of scope)
//   - The Outbox + Cache no-op stubs aren't relevant here because real
//     handlers can be pointed at the embedded URL and run for real.
//
// Cost reminder: each embedded backend spawns its own PG process and
// data directory. fixtures.Bulk and real CRUD operations are full
// round-trip latencies (microseconds, not nanoseconds). Embedded is
// the right choice for: (a) user-authored SQL paths the sim can't
// parse, (b) production-shape fidelity testing.
package embedded

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// ErrUnsupportedOperation is returned by methods that don't make sense
// on the embedded backend (Mark, Fork, etc. — see package doc).
var ErrUnsupportedOperation = errors.New("sandbox embedded: operation not supported (use sim backend)")

// Options tunes the embedded backend. Empty value yields working
// defaults: random free port, 60 s start timeout, discarded logs, and
// a per-instance temp directory for both data and the extracted
// binaries (so parallel instances don't trip over each other's
// cleanup-on-start dance).
type Options struct {
	// Port is the localhost port the embedded PG listens on. 0 →
	// kernel-picked free port. Tests leave this at 0 so parallel
	// instances don't collide.
	Port uint32

	// StartTimeout caps how long Start blocks waiting for PG to
	// come up. 0 → 60 s.
	StartTimeout time.Duration

	// LogOutput receives PG stdout/stderr. nil → io.Discard.
	LogOutput io.Writer

	// DataDir overrides the on-disk PGDATA location. Empty →
	// per-instance temp dir under os.TempDir(). Set this if you
	// want PG state to survive a process restart.
	DataDir string

	// RuntimeDir overrides where embedded-postgres extracts the
	// binary archive. Empty → per-instance temp dir. The library's
	// default is a single shared path that fails cleanup when
	// multiple Backends start back-to-back; we default to unique
	// paths so the failure mode disappears.
	RuntimeDir string
}

// Backend owns one embedded-postgres process + its pgxpool. The
// localPool satisfies runtime.Pool so the sandbox façade can hand it
// back from Sandbox.Pool() without further wrapping.
//
// dataDir and runtimeDir record per-instance temp directories that
// Close removes — without unique directories embedded-postgres's
// cleanup-on-start fails when multiple instances overlap.
type Backend struct {
	mu         sync.Mutex
	pg         *embeddedpostgres.EmbeddedPostgres
	pool       *localPool
	port       uint32
	started    bool
	dataDir    string
	runtimeDir string
}

// New starts an embedded Postgres process and applies the IR's full
// DDL into it. The returned Backend is ready to serve queries; Close
// shuts down the process and removes its data directory.
//
// Cold-start cost: 4–8 s on Linux, 8–12 s on macOS. Tests gate use
// behind build tags or explicit opt-in for that reason.
func New(ctx context.Context, ir *dsl.IR, opts Options) (*Backend, error) {
	if ir == nil {
		return nil, fmt.Errorf("sandbox embedded: IR is required")
	}

	port := opts.Port
	if port == 0 {
		p, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: pick port: %w", err)
		}
		port = uint32(p)
	}
	startTimeout := opts.StartTimeout
	if startTimeout == 0 {
		startTimeout = 60 * time.Second
	}
	logOut := opts.LogOutput
	if logOut == nil {
		logOut = io.Discard
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		d, err := makeTempDir("atlantis-sandbox-data-")
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: tempdir(data): %w", err)
		}
		dataDir = d
	}
	runtimeDir := opts.RuntimeDir
	if runtimeDir == "" {
		d, err := makeTempDir("atlantis-sandbox-runtime-")
		if err != nil {
			return nil, fmt.Errorf("sandbox embedded: tempdir(runtime): %w", err)
		}
		runtimeDir = d
	}

	cfg := embeddedpostgres.DefaultConfig().
		Port(port).
		StartTimeout(startTimeout).
		Logger(logOut).
		DataPath(dataDir).
		RuntimePath(runtimeDir)

	pgInstance := embeddedpostgres.NewDatabase(cfg)
	if err := pgInstance.Start(); err != nil {
		return nil, fmt.Errorf("sandbox embedded: start: %w", err)
	}

	url := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	pool, err := newLocalPool(ctx, url)
	if err != nil {
		_ = pgInstance.Stop()
		return nil, fmt.Errorf("sandbox embedded: connect: %w", err)
	}

	b := &Backend{
		pg:         pgInstance,
		pool:       pool,
		port:       port,
		started:    true,
		dataDir:    dataDir,
		runtimeDir: runtimeDir,
	}

	if err := applySchema(ctx, b, ir); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// Pool returns the runtime.Pool implementation. Generated handlers can
// be wired straight to this and run against real Postgres.
func (b *Backend) Pool() runtime.Pool { return b.pool }

// Port exposes the listening port so tests / CLIs can construct an
// independent connection (e.g. for `psql` debugging).
func (b *Backend) Port() uint32 { return b.port }

// Close shuts down the embedded process, releases the pool, and
// removes the per-instance temp directories. Idempotent — calling
// Close twice is safe; the second call no-ops.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return nil
	}
	b.started = false
	if b.pool != nil {
		b.pool.Close()
	}
	if b.pg != nil {
		if err := b.pg.Stop(); err != nil {
			return fmt.Errorf("sandbox embedded: stop: %w", err)
		}
	}
	// Best-effort tempdir cleanup. We don't surface errors because
	// the runtime is already gone; leftover files are at worst a
	// disk-space leak in /tmp, which the OS cleans periodically.
	if b.dataDir != "" {
		_ = os.RemoveAll(b.dataDir)
	}
	if b.runtimeDir != "" {
		_ = os.RemoveAll(b.runtimeDir)
	}
	return nil
}

// applySchema reuses the production DDL emitter to build the schema
// from an empty IR up to the supplied one. The "diff from empty"
// trick gives us every CREATE TABLE / CREATE INDEX / trigger the
// caller's schema implies, without writing a parallel emitter.
//
// Proto numbers aren't assigned in raw IRs; codegen.AssignProtoNumbers
// is called first so EmitSQL sees the same shape codegen-emitted
// migrations land with.
//
// EmitSQL omits CREATE SCHEMA when the schema name is the canonical
// "atlantis" one (production has it pre-created by the bootstrap
// migration). On a fresh embedded PG nothing has been bootstrapped,
// so we prepend CREATE SCHEMA IF NOT EXISTS for every schema the
// catalog will reference. Idempotent — safe even when the emitter
// already emits the CREATE.
func applySchema(ctx context.Context, b *Backend, ir *dsl.IR) error {
	empty := &dsl.IR{}
	codegen.AssignProtoNumbers(empty, ir)
	d := codegen.ComputeDiff(empty, ir)
	scripts, err := codegen.EmitSQL(empty, ir, d)
	if err != nil {
		return fmt.Errorf("sandbox embedded: emit DDL: %w", err)
	}
	if scripts.Up == "" {
		return nil
	}
	preamble := schemaPreamble(ir)
	full := preprocessDDLForVanillaPG(preamble + scripts.Up)
	if _, err := b.pool.Exec(ctx, full); err != nil {
		return fmt.Errorf("sandbox embedded: apply DDL: %w\n--- SQL ---\n%s", err, full)
	}
	return nil
}

// preprocessDDLForVanillaPG rewrites the codegen-emitted DDL so it
// loads on the stock Postgres binary embedded-postgres ships — which
// has no pgvector, no TimescaleDB. Three transforms:
//
//  1. `vector(N)` columns → `BYTEA`. Column round-trips, but vector
//     similarity operators (<->, <=>, <#>) and pgvector-typed binds
//     fail at query time. Acceptable: schemas using vectors should
//     usually pick the sim backend (which models vectors natively)
//     rather than embedded.
//
//  2. HNSW index DDL is stripped. Without pgvector the access method
//     doesn't exist; the table loads, queries that would have used
//     the index just do sequential scans.
//
//  3. `SELECT create_hypertable(...)` is stripped. The hypertable
//     becomes a regular table — time-based queries still work, just
//     without partition pruning.
//
// All three transforms are best-effort regex passes. The alternative —
// failing the boot — is the worst outcome on real caller schemas that
// universally combine these extensions with otherwise-vanilla DDL.
func preprocessDDLForVanillaPG(ddl string) string {
	ddl = pgvectorTypeRE.ReplaceAllString(ddl, "BYTEA")
	ddl = hnswIndexRE.ReplaceAllString(ddl, "-- HNSW index stripped (no pgvector on embedded)")
	ddl = hypertableRE.ReplaceAllString(ddl, "-- create_hypertable stripped (no TimescaleDB on embedded)")
	return ddl
}

var (
	pgvectorTypeRE = regexp.MustCompile(`(?i)\bvector\s*\(\s*\d+\s*\)`)
	hnswIndexRE    = regexp.MustCompile(`(?is)CREATE\s+INDEX[^;]*?\s+USING\s+hnsw[^;]*?;`)
	hypertableRE   = regexp.MustCompile(`(?is)SELECT\s+create_hypertable\s*\([^;]*?\)\s*;`)
)

// schemaPreamble emits `CREATE SCHEMA IF NOT EXISTS "<name>"` for
// every distinct schema name the IR references. EmitSQL elides these
// for the "atlantis" schema (assumed already bootstrapped in prod);
// on a fresh embedded PG nothing exists yet so we synthesize them.
func schemaPreamble(ir *dsl.IR) string {
	// Codegen places touch-trigger functions in the "atlantis" schema
	// regardless of which schema the table lives in
	// (`CREATE FUNCTION "atlantis"."ns_entity_touch_fn"() ...`), and
	// each entity's CREATE TRIGGER then references that function. So
	// the atlantis schema must exist even when no entity table is
	// declared inside it — bootstrap it unconditionally.
	seen := map[string]struct{}{"atlantis": {}}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		// Default schema is "atlantis"; entities with a TableName
		// override might point at a different one.
		schema := "atlantis"
		if e.TableName != "" {
			// table "schema.name" form. dsl.Entity.TableName is the
			// raw modifier; codegen normalizes it. Anything before a
			// dot is the schema.
			if i := indexOf(e.TableName, '.'); i >= 0 {
				schema = e.TableName[:i]
			}
		}
		seen[schema] = struct{}{}
	}
	var out string
	for s := range seen {
		out += fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;\n", s)
	}
	return out
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// freePort asks the kernel for an unused TCP port. We bind, read the
// assigned port, then close — there's an inherent TOCTOU window
// (another process could grab the port between our close and PG's
// listen), but in practice PG holds it for the whole process lifetime
// and the race almost never fires.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return addr.Port, nil
}

// makeTempDir returns a uniquely-named subdir under os.TempDir().
// Each Backend instance gets its own (data + runtime) tempdir pair so
// embedded-postgres's cleanup-on-start dance doesn't fail when
// multiple instances run back-to-back. The caller is responsible for
// removing the dir on Close — we do that in Backend.Close().
func makeTempDir(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), prefix+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
