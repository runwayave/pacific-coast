package sandbox

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// Fork returns n child sandboxes that share the current state but
// diverge independently on every write. Implementation uses CoW
// pointer-sharing — the children start with the same rowMap pointers
// as the parent, all marked shared so the first write on either side
// clones the affected table. Cost: O(num_tables × n), independent of
// row count.
//
// Catalog is shared by reference (catalogs are read-mostly +
// register-once); each child gets its own Cache and Outbox stubs so
// test code can assert against one branch's invalidation log without
// seeing sibling activity.
//
// Fork is the right primitive for parallel state-space exploration:
// take a base state, fork into N branches, evaluate independently,
// compare results via Inspect.Diff or per-branch assertions. Marks
// are the right primitive for sequential rollback within one branch.
func (s *Sandbox) Fork(n int) ([]*Sandbox, error) {
	if s.embedded != nil {
		return nil, ErrFeatureRequiresSim
	}
	if n < 0 {
		return nil, fmt.Errorf("sandbox: Fork called with negative n=%d", n)
	}
	if n == 0 {
		return nil, nil
	}
	shared := s.pool.ForkPointers()
	out := make([]*Sandbox, n)
	for i := range n {
		out[i] = s.forkOne(shared)
	}
	return out, nil
}

// forkOne builds one child sandbox with rowMap pointers shared from
// the parent's snapshot. The catalog reference is reused — every
// fork has the same schema by construction.
func (s *Sandbox) forkOne(shared map[string]*sim.RowMap) *Sandbox {
	child := &Sandbox{
		opts: s.opts,
		pool: sim.NewPoolWithOptions(s.pool.Catalog(), sim.PoolOptions{
			Clock: s.opts.Clock,
			Warn:  s.opts.Warn,
		}),
		cache:  NewCache(),
		outbox: NewOutbox(),
	}
	child.pool.InstallSharedPointers(shared)
	return child
}
