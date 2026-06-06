package conformance_test

// The single test entry point for the conformance harness. RunAll
// dispatches every Scenario across every available Backend, so
// failures localize to a precise (backend, scenario) cell:
//
//	--- FAIL: TestConformance
//	    --- FAIL: TestConformance/embedded
//	        --- FAIL: TestConformance/embedded/keyset_cursor_mixed_direction
//	            scenarios.go:NN: keyset page: [...] want [...]
//
// Run with -short to skip embedded (~60 s saved at the cost of
// fidelity-matrix enforcement). CI runs without -short.

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/conformance"
)

func TestConformance(t *testing.T) {
	conformance.RunAll(t)
}
