package sandbox

import (
	"context"
	"sync"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Outbox is the runtime.Outbox stub. Generated Create/Update/Delete
// handlers call Enqueue inside the tx before commit, then
// EnqueueGenerationBump afterward — for the sim we silently record both
// without driving a real worker or NOTIFY-channel pipeline.
//
// Tests that want to assert "the handler emitted the right invalidation"
// inspect Recorded(); everything else can ignore the outbox.
type Outbox struct {
	mu       sync.Mutex
	enqueues []EnqueueCall
	bumps    []BumpCall
}

// EnqueueCall is a recorded Outbox.Enqueue invocation.
type EnqueueCall struct {
	Entity     string
	ID         string
	NewVersion int64
}

// BumpCall is a recorded Outbox.EnqueueGenerationBump invocation.
type BumpCall struct {
	Entity string
}

// NewOutbox returns an empty Outbox stub.
func NewOutbox() *Outbox { return &Outbox{} }

// Compile-time check.
var _ runtime.Outbox = (*Outbox)(nil)

// Enqueue records the call. The tx argument is unused because the sim's
// outbox doesn't actually write to a backing table — there's nothing
// for the worker to pick up. Recording happens unconditionally so the
// caller doesn't lose the invalidation intent if the tx later rolls
// back; that mirrors production where the row IS written inside the
// tx and a rollback removes it. Per-tx rollback is not implemented
// on sim; when it is, this function will need to drop recorded
// enqueues on rollback to match production semantics.
func (o *Outbox) Enqueue(_ context.Context, _ runtime.Tx, entity, id string, newVersion int64) error {
	o.mu.Lock()
	o.enqueues = append(o.enqueues, EnqueueCall{Entity: entity, ID: id, NewVersion: newVersion})
	o.mu.Unlock()
	return nil
}

// EnqueueGenerationBump records a per-entity bump.
func (o *Outbox) EnqueueGenerationBump(_ context.Context, _ runtime.Tx, entity string) error {
	o.mu.Lock()
	o.bumps = append(o.bumps, BumpCall{Entity: entity})
	o.mu.Unlock()
	return nil
}

// Recorded returns a snapshot of recorded calls. Tests assert on the
// returned slice; the original log keeps growing in-place.
func (o *Outbox) Recorded() (enqueues []EnqueueCall, bumps []BumpCall) {
	o.mu.Lock()
	defer o.mu.Unlock()
	enqueues = make([]EnqueueCall, len(o.enqueues))
	copy(enqueues, o.enqueues)
	bumps = make([]BumpCall, len(o.bumps))
	copy(bumps, o.bumps)
	return enqueues, bumps
}

// Reset clears recorded calls. Useful between sub-tests.
func (o *Outbox) Reset() {
	o.mu.Lock()
	o.enqueues = nil
	o.bumps = nil
	o.mu.Unlock()
}
