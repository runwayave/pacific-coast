package sandbox

import (
	"testing"
)

// NewT is the canonical Go test helper. It constructs a Sandbox with
// the given options, registers a t.Cleanup to release any resources,
// and returns the handle. Tests use it like:
//
//	sb := atl.NewT(t, atl.Options{IR: testIR})
//	srv := consumerpb.NewAccountServer(sb.Pool(), sb.Cache())
//	// ... call RPCs
//
// The helper is named NewT so it reads idiomatically when the package
// is imported as `atl`.
//
// On construction failure the helper calls t.Fatal so tests don't need
// to repeat the err != nil guard for every case.
func NewT(t *testing.T, opts Options) *Sandbox {
	t.Helper()
	sb, err := New(opts)
	if err != nil {
		t.Fatalf("sandbox.NewT: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Errorf("sandbox.Close: %v", err)
		}
	})
	return sb
}
