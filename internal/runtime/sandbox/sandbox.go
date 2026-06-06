// Package sandbox is the atlantis schema-true sandbox runtime.
//
// Two backends now share the same surface:
//
//   - sim (default): pure-Go in-memory simulator under
//     internal/runtime/sandbox/sim. Sub-millisecond boot, whitelist
//     SQL surface, full support for time-travel (Mark/RestoreTo) and
//     forks. This is the default backend for sandbox.New.
//
//   - embedded: real Postgres via fergusstrange/embedded-postgres at
//     internal/runtime/sandbox/embedded. Multi-second boot, 100%
//     fidelity. The fidelity escape hatch — used automatically when
//     auto-routing detects user-authored SQL (custom query/procedure/
//     hypertable) the sim's whitelist can't honor, or selected
//     explicitly via Options.Backend = BackendEmbedded.
//
// Both backends satisfy the runtime.Pool contract that generated
// handlers depend on. Mark, RestoreTo, and Fork are sim-only (the
// fidelity matrix documents the boundary); the surface returns a
// typed ErrFeatureRequiresSim when called on an embedded sandbox.
package sandbox

import (
	"context"
	"errors"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/embedded"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// ErrFeatureRequiresSim is returned by sim-only features (Mark / Fork)
// when invoked on a sandbox backed by the embedded PG runtime.
var ErrFeatureRequiresSim = errors.New("sandbox: feature requires the sim backend; embedded does not support it")

// Sandbox is the concrete handle returned by New. It owns whichever
// backend the Options selected. The Cache and Outbox stubs are kept
// alongside both backends so generated handlers behave identically
// regardless of which one is in play — Cache always misses, Outbox
// records calls without driving a real worker.
type Sandbox struct {
	opts     Options
	pool     *sim.Pool         // non-nil for sim backend
	embedded *embedded.Backend // non-nil for embedded backend
	cache    *Cache
	outbox   *Outbox
}

// New constructs a sandbox. Backend selection:
//
//   - BackendSim (default): pure-Go simulator.
//   - BackendEmbedded: real Postgres via embedded-postgres. Multi-
//     second cold start; the fidelity escape hatch.
//   - BackendAuto: routes to embedded when opts.IR contains user-
//     authored SQL (custom query/procedure/hypertable blocks) — those
//     paths land outside the sim's whitelist and need real PG.
//     Otherwise sim.
//
// When opts.IR is non-nil:
//   - sim path: New calls sb.LoadIR(opts.IR) to register the catalog.
//   - embedded path: the embedded backend applies the IR's full DDL
//     at boot via the existing codegen emitter.
func New(opts Options) (*Sandbox, error) {
	opts = opts.withDefaults()

	backend := opts.Backend
	if backend == BackendAuto {
		backend = chooseBackend(opts.IR)
	}

	switch backend {
	case BackendSim:
		return newSim(opts)
	case BackendEmbedded:
		return newEmbedded(opts)
	}
	return nil, errBackendUnknown(opts.Backend)
}

// chooseBackend implements the auto-routing logic. The plan calls
// out three trigger conditions for routing to embedded:
//
//   - Custom query block (user-authored SQL the sim's whitelist
//     doesn't parse — LATERAL, recursive CTE, WITH ORDINALITY etc.)
//   - Custom procedure block (same reason)
//   - Hypertable entity (would silently degrade to a regular table on
//     sim; embedded preserves chunk-aware semantics)
//
// Anything else — including a fully-loaded entity-only IR — routes
// to sim. Sim boots in under a millisecond; routing the empty-catalog and entity-only cases there keeps the common path off the embedded-pg cold start.
// nil IR routes to sim too; the empty-catalog test path stays cheap.
func chooseBackend(ir *dsl.IR) Backend {
	if ir == nil {
		return BackendSim
	}
	if len(ir.Queries) > 0 || len(ir.Procedures) > 0 {
		return BackendEmbedded
	}
	for i := range ir.Entities {
		if ir.Entities[i].Kind == dsl.EntityKindHypertable {
			return BackendEmbedded
		}
	}
	return BackendSim
}

func newSim(opts Options) (*Sandbox, error) {
	cat := sim.NewCatalog()
	pool := sim.NewPoolWithOptions(cat, sim.PoolOptions{
		Clock: opts.Clock,
		Warn:  opts.Warn,
	})
	sb := &Sandbox{
		opts:   opts,
		pool:   pool,
		cache:  NewCache(),
		outbox: NewOutbox(),
	}
	if opts.IR != nil {
		if err := sb.LoadIR(opts.IR); err != nil {
			return nil, err
		}
	}
	return sb, nil
}

func newEmbedded(opts Options) (*Sandbox, error) {
	if opts.IR == nil {
		return nil, errors.New("sandbox: BackendEmbedded requires Options.IR")
	}
	ctx := context.Background()
	be, err := embedded.New(ctx, opts.IR, embedded.Options{})
	if err != nil {
		return nil, err
	}
	return &Sandbox{
		opts:     opts,
		embedded: be,
		cache:    NewCache(),
		outbox:   NewOutbox(),
	}, nil
}

// Pool returns the runtime.Pool the generated server code consumes.
// Routes to the active backend.
func (s *Sandbox) Pool() runtime.Pool {
	if s.embedded != nil {
		return s.embedded.Pool()
	}
	return s.pool
}

// Cache returns the runtime.Cache stub.
func (s *Sandbox) Cache() runtime.Cache { return s.cache }

// CacheStub returns the concrete sandbox Cache for tests.
func (s *Sandbox) CacheStub() *Cache { return s.cache }

// Outbox returns the runtime.Outbox stub.
func (s *Sandbox) Outbox() runtime.Outbox { return s.outbox }

// OutboxStub returns the concrete sandbox Outbox for tests.
func (s *Sandbox) OutboxStub() *Outbox { return s.outbox }

// Catalog exposes the sim's catalog when the sandbox is sim-backed.
// Returns nil for embedded sandboxes — the embedded backend's schema
// lives in a real PG instance reachable via Pool().
func (s *Sandbox) Catalog() *sim.Catalog {
	if s.pool == nil {
		return nil
	}
	return s.pool.Catalog()
}

// IsEmbedded reports whether this sandbox is backed by the embedded
// Postgres process. Callers use this to skip sim-only assertions
// (Mark/Fork/Snapshot) or to pull the listening port from the
// underlying backend.
func (s *Sandbox) IsEmbedded() bool { return s.embedded != nil }

// EmbeddedBackend returns the underlying embedded backend or nil when
// the sandbox is sim-backed. Exposed mostly for the CLI / tests that
// want the listening port.
func (s *Sandbox) EmbeddedBackend() *embedded.Backend { return s.embedded }

// Close releases any resources held by the sandbox. For embedded
// sandboxes this stops the PG process and removes its data
// directory. Idempotent.
func (s *Sandbox) Close() error {
	if s.embedded != nil {
		return s.embedded.Close()
	}
	return nil
}
