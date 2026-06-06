// Package conformance is the differential safety harness the
// published fidelity matrix promises. Every scenario in this package
// runs against the sim AND the embedded backend; any divergence
// fails the test, so the matrix can never silently drift.
//
// The plan calls this out as the "safety contract that keeps the
// fidelity matrix honest." Concretely: when codegen emits a new SQL
// shape, the scenario covering it lands here. When the sim's
// whitelist parser is extended, the new scenario exercises both
// backends — sim must execute it equivalently to embedded, or the
// new construct goes on the explicit-not-supported list.
//
// Design choices:
//
//   - Scenarios assert ABSOLUTE properties (row counts, scanned
//     values, error types), not cross-backend equality of values.
//     This sidesteps the "PG timestamptz is µs, Go is ns" class of
//     spurious divergences. Cross-backend equality is enforced
//     implicitly: both backends must satisfy the same assertions.
//
//   - Each scenario boots its own sandbox. Embedded is slow
//     (~4 s per boot) so the full embedded run takes ~60 s; that's
//     the published budget for production-fidelity testing.
//
//   - `-short` skips embedded so day-to-day dev (go test ./...) stays
//     fast. CI runs without `-short` to enforce the contract.
//
//   - Scenarios that only make sense on one backend declare it via
//     SkipOn — e.g. recursive CTE only runs on embedded; Mark/Fork
//     only run on sim.
package conformance

import (
	"slices"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// Backend identifies one runtime backend under test. Name() is the
// human label used by t.Run subtests; SandboxBackend returns the
// enum value New() consumes.
type Backend interface {
	Name() string
	SandboxBackend() sandbox.Backend
}

// Sim is the pure-Go simulator backend.
type Sim struct{}

func (Sim) Name() string                    { return "sim" }
func (Sim) SandboxBackend() sandbox.Backend { return sandbox.BackendSim }

// Embedded is the real-PG fidelity backend. Tests using this take
// ~4 s per boot — `-short` skips them.
type Embedded struct{}

func (Embedded) Name() string                    { return "embedded" }
func (Embedded) SandboxBackend() sandbox.Backend { return sandbox.BackendEmbedded }

// Scenario is one differential test case. Run is invoked once per
// available backend with a freshly-booted sandbox; assertions inside
// Run determine pass/fail. The same Run runs against every backend
// the scenario doesn't list in SkipOn — the differential property
// is enforced by both runs passing the same assertions.
type Scenario struct {
	Name string

	// IR returns a fresh IR per call so concurrent test runs don't
	// share state through pointer aliasing.
	IR func() *dsl.IR

	// Run carries the actual assertions. It receives a Sandbox booted
	// for the current backend; sandbox cleanup is wired via t.Cleanup
	// before Run is invoked.
	Run func(t *testing.T, sb *sandbox.Sandbox)

	// SkipOn lists backend names this scenario should skip. Empty →
	// runs against every backend. Examples:
	//   - "embedded": scenario uses sim-only features (Mark, Fork)
	//   - "sim": scenario uses constructs only embedded supports
	//     (recursive CTE, LATERAL, etc.)
	SkipOn []string
}

// boot launches a sandbox for the given backend + IR and registers
// cleanup. Used by the test driver below; scenarios shouldn't call
// it directly.
func boot(t *testing.T, b Backend, ir *dsl.IR) *sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.New(sandbox.Options{
		Backend: b.SandboxBackend(),
		IR:      ir,
	})
	if err != nil {
		t.Fatalf("boot %s: %v", b.Name(), err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// availableBackends returns the backends to test under current
// conditions. Sim always runs; embedded is gated on -short being
// off (real PG boot is too slow for fast local runs).
func availableBackends() []Backend {
	if testing.Short() {
		return []Backend{Sim{}}
	}
	return []Backend{Sim{}, Embedded{}}
}

// RunAll is the test driver. Each backend gets a subtest; each
// scenario inside that subtest is its own t.Run so failures localize
// to a single (backend, scenario) cell. Same-named scenarios under
// different backends produce comparable subtests, making "this passes
// on sim but fails on embedded" instantly readable in test output.
func RunAll(t *testing.T) {
	t.Helper()
	for _, b := range availableBackends() {
		b := b
		t.Run(b.Name(), func(t *testing.T) {
			for _, sc := range Scenarios {
				sc := sc
				if slices.Contains(sc.SkipOn, b.Name()) {
					continue
				}
				t.Run(sc.Name, func(t *testing.T) {
					sb := boot(t, b, sc.IR())
					sc.Run(t, sb)
				})
			}
		})
	}
}
