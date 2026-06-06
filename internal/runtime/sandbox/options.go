package sandbox

import (
	"sync/atomic"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Backend selects which simulator drives the sandbox. Auto routes per
// the IR: a caller schema with any custom query/procedure/hypertable
// block goes to the embedded backend (the sim's whitelist can't honor
// them); anything else routes to sim. Explicit values force one or
// the other.
type Backend string

const (
	BackendAuto     Backend = "auto"
	BackendSim      Backend = "sim"
	BackendEmbedded Backend = "embedded"
)

// Determinism controls whether the sandbox guarantees bit-for-bit
// reproducible runs given identical Seed + identical inputs. Strict
// mode replaces wall-clock time with a deterministic fake clock that
// advances 1ms per now() call (or Seed-derived; see below), and the
// sim's scan paths already iterate in sorted order. Future slices
// extend Strict to seeded id.Generate* shims.
//
// Off (default) lets the sandbox use real wall-clock time — fine for
// most test code; required for any test that asserts on real-time
// durations.
type Determinism uint8

const (
	DeterminismOff Determinism = iota
	DeterminismStrict
)

// Options is the construction-time configuration for a Sandbox. Most
// fields have zero-value defaults that produce a working sim; callers
// only set what they need.
type Options struct {
	// Backend selects the underlying simulator. Default: BackendSim.
	Backend Backend

	// Clock returns "now" for the simulator. nil → time.Now. Explicit
	// clocks supersede Determinism; if both are set the explicit
	// Clock wins so test code that pins a specific moment stays
	// uncoupled from the determinism toggle.
	Clock func() time.Time

	// IR, when non-nil, is loaded into the catalog at New. Equivalent
	// to constructing the sandbox empty and calling sb.LoadIR(ir).
	IR *dsl.IR

	// Determinism controls whether the sandbox produces bit-for-bit
	// reproducible runs. When Strict and Clock is nil, the sandbox
	// installs a Seed-driven monotonic fake clock; sim scans already
	// iterate in sorted order, so the executed statement sequence
	// becomes deterministic by construction.
	Determinism Determinism

	// Seed is the initial counter for the deterministic fake clock and
	// (when wired) the seed for id.Generate* shims. Zero is a valid
	// seed and is treated as "start at epoch + 0ms."
	Seed int64

	// Warn receives one-shot fidelity warnings emitted by the sim —
	// the published fidelity matrix promises a runtime notice the
	// first time a hypertable (or other stubbed surface) is hit. nil
	// → log.Printf with "sandbox warn: " prefix. Tests set this to a
	// slice-collector to assert lines without parsing log output.
	Warn func(string)
}

// withDefaults fills in the zero values. Returning a copy keeps
// Options usable as a value the caller might reuse — they get back a
// fresh struct rather than a mutation of theirs.
//
// Clock resolution order:
//  1. Explicit Clock (caller pinned a moment) — wins.
//  2. DeterminismStrict + no explicit clock → monotonic fake clock
//     seeded from Seed; advances 1 ms per now() call.
//  3. Default → time.Now.
func (o Options) withDefaults() Options {
	if o.Backend == "" {
		o.Backend = BackendSim
	}
	if o.Clock == nil {
		if o.Determinism == DeterminismStrict {
			o.Clock = newDeterministicClock(o.Seed)
		} else {
			o.Clock = time.Now
		}
	}
	return o
}

// newDeterministicClock returns a now() function that advances by 1ms
// per call starting from `epoch + seed ms`. Independent atomics per
// sandbox guarantee two sandboxes with the same seed produce
// identical now() sequences, regardless of interleaving — the
// foundation of the StrictDeterministic guarantee.
func newDeterministicClock(seed int64) func() time.Time {
	var counter atomic.Int64
	counter.Store(seed)
	epoch := time.Unix(0, 0).UTC()
	return func() time.Time {
		n := counter.Add(1)
		return epoch.Add(time.Duration(n) * time.Millisecond)
	}
}
