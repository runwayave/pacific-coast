package sandbox

import (
	"errors"
	"sync"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// Mark is an in-process state capture. Distinct from Snapshot (which
// produces a portable byte blob): a Mark lives in memory, can be
// restored without serialization, and exists only for the lifetime
// of the Sandbox that produced it.
//
// The expected idiom is: capture marks at decision points, mutate
// freely, restore to undo a branch and try another. Agent loops
// running thousands of "branch / try / rewind" cycles per second
// hold dozens of marks at once; Mark itself carries no large API
// surface so passing it around is cheap.
//
// Implementation: the underlying sim.Pool exposes per-table CoW
// pointer capture. Mark just records those pointers and tags each
// rowMap as "shared" so the next write clone-on-writes. Cost is
// O(num_tables), not O(num_rows).
type Mark struct {
	owner    *Sandbox
	captured map[string]*sim.RowMap
}

// ErrMarkOwnerMismatch is returned by RestoreTo when the Mark was
// produced by a different Sandbox. Marks are intentionally
// non-portable across sandboxes — Fork is the right primitive when
// one needs N independent state branches.
var ErrMarkOwnerMismatch = errors.New("sandbox: mark belongs to a different sandbox")

// marksMu serializes Mark / RestoreTo at the package level. The
// underlying sim.Pool already serializes writes per-table; this
// outer lock just keeps the (capture, restore) pair atomic with
// respect to other marks on the same sandbox.
var marksMu sync.Mutex

// Mark captures the sandbox's current state into an in-memory token.
// The returned Mark is independent of subsequent writes; restoring
// it later rewinds to exactly the captured moment.
//
// sim-only: embedded sandboxes return nil because pg_dump-style
// snapshots are too slow for the agent-loop budget. Use Snapshot()
// for cross-process bytes on embedded.
//
// Cost: O(num_tables). The captured rowMap pointers stay valid as
// long as the Mark reference is held; the underlying maps are
// retained via the pointer and GC'd when the Mark goes out of scope.
func (s *Sandbox) Mark() *Mark {
	if s.embedded != nil {
		return nil
	}
	marksMu.Lock()
	defer marksMu.Unlock()
	return &Mark{
		owner:    s,
		captured: s.pool.CapturePointers(),
	}
}

// RestoreTo rewinds the sandbox to the state Mark captured. Tables
// that were created after the mark are cleared; tables in the mark
// that have been modified are reset by pointer swap.
//
// Returns ErrMarkOwnerMismatch if the mark was created by a
// different sandbox. ErrFeatureRequiresSim on embedded sandboxes.
//
// Cost: O(num_tables) — pointer swap per table.
func (s *Sandbox) RestoreTo(m *Mark) error {
	if s.embedded != nil {
		return ErrFeatureRequiresSim
	}
	if m == nil {
		return errors.New("sandbox: RestoreTo nil mark")
	}
	if m.owner != s {
		return ErrMarkOwnerMismatch
	}
	marksMu.Lock()
	defer marksMu.Unlock()
	s.pool.ReplaceFromPointers(m.captured)
	return nil
}
