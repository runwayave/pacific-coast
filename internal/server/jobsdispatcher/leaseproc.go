// Per-session batched lease extension processor. Heartbeat /
// Checkpoint receive paths enqueue job IDs onto a buffered channel;
// this goroutine drains them in batches of up to flushInterval (250ms)
// and runs one jobs.ExtendLease per batch. Replaces the previous
// synchronous-PG-hit-per-envelope path that wedged the stream's recv
// loop under heavy load.
//
// Why this matters: with the synchronous path, 16 concurrent handlers
// emitting heartbeats every 10s produced a steady stream of single-id
// UPDATEs. When PG was momentarily slow, the recv goroutine blocked on
// ExtendLease → the stream's recv buffer filled → the SDK's stream.Send
// blocked → heartbeats backed up in the SDK's sendCh → enqueueSend
// dropped them (non-blocking default:). Server stopped seeing
// heartbeats, lease expired, jobs were revoked + re-dispatched. The
// batched processor breaks this feedback loop: enqueueing is O(1),
// the recv loop never blocks on PG.

package jobsdispatcher

import (
	"context"
	"time"

	"github.com/rachitkumar205/atlantis/clients/go/jobs"
)

// leaseFlushInterval is the cadence at which a session's lease
// processor coalesces enqueued job IDs into a single ExtendLease call.
// 250ms gives one batch per heartbeat tick under typical 10s heartbeat
// cadence, and amortises bursts (e.g., 16 handlers heartbeating in
// the same millisecond after a network jitter recovery).
const leaseFlushInterval = 250 * time.Millisecond

// leaseEnqueueCap bounds the per-session pending-bump queue. Sized
// generously enough that a momentary processor stall (e.g., during a
// long flush) doesn't drop bumps for a worker running near MaxInFlight.
const leaseEnqueueCap = 1024

// leaseProcessor runs once per session. It owns a buffered chan of job
// IDs the dispatcher wants to bump and emits one batched ExtendLease
// per flush tick. Shuts down when the supplied ctx cancels (typically
// the WorkerSession stream's context); a final flush drains any
// pending IDs before return so a clean stream close doesn't leak
// almost-expired leases.
//
// Per-session, not global, because each session has its own claimedBy
// string. ExtendLease's WHERE-predicate guards against bumping leases
// reassigned to peers, and the claimedBy is constant for a session's
// lifetime — so one bucket per session is the natural unit.
func (d *Dispatcher) runLeaseProcessor(ctx context.Context, s *session) {
	ticker := time.NewTicker(leaseFlushInterval)
	defer ticker.Stop()

	// Batch is rebuilt each tick. A small slab avoids per-tick alloc
	// while accommodating the typical burst size.
	batch := make([]int64, 0, 32)

	drain := func() {
		// Pull whatever's queued without blocking.
		for {
			select {
			case id := <-s.leasePendingCh:
				batch = append(batch, id)
			default:
				return
			}
		}
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Group by lease duration so per-job `heartbeat <dur>` overrides
		// land on the SQL UPDATE, not just on the initial claim. Most
		// sessions have no overrides — one group; we emit one
		// ExtendLease call. Sessions with mixed durations emit one
		// ExtendLease per distinct duration.
		groups := make(map[time.Duration][]int64, 1)
		s.inflightMu.Lock()
		for _, id := range batch {
			row, ok := s.inflight[id]
			if !ok {
				// Row was completed / revoked between enqueue and
				// flush. Skip — extending a lease we no longer own
				// would no-op (claimedBy mismatch) but emits a
				// pointless UPDATE.
				continue
			}
			dur := s.leaseDurFor(row.jobName, d.cfg.HeartbeatBudget)
			groups[dur] = append(groups[dur], id)
		}
		s.inflightMu.Unlock()
		for dur, ids := range groups {
			if err := jobs.ExtendLease(ctx, d.pool, ids, s.claimedBy(), dur); err != nil {
				d.cfg.Logger.Warn("dispatcher: batched lease extend",
					"session", s.id, "batch_size", len(ids), "dur", dur, "err", err)
			}
		}
		// Reset slab in place; capacity persists for reuse.
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			drain()
			flush()
			return
		case id := <-s.leasePendingCh:
			batch = append(batch, id)
			// Opportunistically drain any siblings queued in the same
			// quantum so a thundering-herd burst becomes one batch
			// instead of many.
			drain()
		case <-ticker.C:
			flush()
		}
	}
}

// enqueueLeaseBump pushes a job ID onto the session's lease-processor
// channel. Returns immediately; the actual SQL UPDATE happens
// asynchronously on the next flush tick. Non-blocking send: if the
// channel is full (rare; the cap is large), the bump is dropped with
// a warn log. A dropped bump is recovered on the next heartbeat —
// the SDK's runHeartbeat re-sends every leaseFlushInterval / 30 (the
// heartbeat cadence).
func (d *Dispatcher) enqueueLeaseBump(s *session, jobID int64) {
	select {
	case s.leasePendingCh <- jobID:
	default:
		d.cfg.Logger.Warn("dispatcher: lease bump dropped (processor backlogged)",
			"session", s.id, "job_id", jobID)
	}
}
