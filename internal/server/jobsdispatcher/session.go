// Per-worker session state. One session per connected WorkerSession
// stream. The dispatcher's drain loop pushes claimed rows into a
// session's outbox; the session's send goroutine forwards them as
// Dispatch envelopes; the session's recv goroutine handles
// Ack/Heartbeat/Complete/Fail/Goodbye envelopes back from the worker.
//
// Lifecycle: register on `Open` validation success → drain loop sees
// the session in `dispatcher.sessions` → routes rows → session is
// removed on Recv error or operator Drain/Evict → release loop
// returns every still-in-flight row to pending.

package jobsdispatcher

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// session is a single connected worker. Most fields are set at
// registration time and never mutated; the in-flight bookkeeping is
// protected by inflightMu.
type session struct {
	id      string
	caller  string
	aliases []string // operator-configured aliases at Open; immutable thereafter
	queue   string
	podID   string
	version string

	// jobNames is the worker-declared set of canonical job ids the
	// session handles. Used by dispatcher.pickSession to route only
	// rows the worker can handle.
	jobNames    map[string]struct{}
	maxInFlight int

	// outbox is the dispatch send queue. Buffered to maxInFlight + a
	// small headroom so a brief flow-control stall doesn't drop us
	// below saturation. Send blocks longer than leaseTTL/4 → treat
	// the worker as dead (handled in runSender).
	outbox chan *DispatchEnvelope

	// inflightMu guards inflight + inflightAck. We use a fast atomic
	// counter for the hot path (read inflight count to compute slot
	// budget without locking) plus the mu-guarded maps for accurate
	// release-on-disconnect.
	inflightMu      sync.Mutex
	inflight        map[int64]inflightRow
	inflightCounter atomic.Int32

	// drained is set when the session is being gracefully drained
	// (operator Drain or server shutdown). The drain loop stops
	// pushing new rows once this flips.
	drained atomic.Bool

	// Per-session counters. Bumped at every state transition. Read by
	// SnapshotSessions / GetSession for the console's "Workers" tab.
	// Atomic so the read path doesn't take inflightMu.
	cntDispatched atomic.Int64
	cntCompleted  atomic.Int64
	cntFailed     atomic.Int64
	cntRevoked    atomic.Int64

	// lastHeartbeatNS is the wall-clock nanos of the most recent
	// Heartbeat envelope received from the worker. The console
	// renders a "stale heartbeat" pill when now - this exceeds
	// 2× HeartbeatMS. Atomic int64 to avoid locking on every bump.
	lastHeartbeatNS atomic.Int64

	connectedAt time.Time
	closeOnce   sync.Once
	closeCh     chan struct{}

	// events is the per-session ring buffer surfaced by
	// GetWorkerSession admin RPC (PR 3). Capacity bounded so a
	// long-lived session can't accumulate unbounded history.
	eventsMu sync.Mutex
	events   []sessionEvent
}

type inflightRow struct {
	jobID        int64
	jobName      string
	dispatchedAt time.Time
	leaseUntil   time.Time
	ackReceived  bool
	ackBy        time.Time // expected-by deadline for missing-Ack detection
}

type sessionEvent struct {
	At      time.Time
	Kind    string // dispatched | acked | completed | failed | revoked | authz_rejected | heartbeat_received | protocol_violation
	JobID   int64  // 0 when not job-scoped
	JobName string // "" when not job-scoped
	Note    string
}

const sessionEventCap = 50

func newSession(open *OpenSession, caller string, aliases []string) *session {
	jobNames := make(map[string]struct{}, len(open.JobNames))
	for _, n := range open.JobNames {
		jobNames[n] = struct{}{}
	}
	maxIF := open.MaxInFlight
	if maxIF < MinMaxInFlight {
		maxIF = MinMaxInFlight
	}
	if maxIF > MaxMaxInFlight {
		maxIF = MaxMaxInFlight
	}
	// Defensive copy: caller could otherwise mutate the slice after
	// session registration. The alias set is frozen at Open per the
	// "stable for the stream's lifetime" contract.
	var aliasCopy []string
	if len(aliases) > 0 {
		aliasCopy = append([]string(nil), aliases...)
	}
	s := &session{
		id:          uuid.NewString(),
		caller:      caller,
		aliases:     aliasCopy,
		queue:       open.Queue,
		podID:       open.PodID,
		version:     open.Version,
		jobNames:    jobNames,
		maxInFlight: maxIF,
		outbox:      make(chan *DispatchEnvelope, maxIF+4),
		inflight:    make(map[int64]inflightRow, maxIF),
		connectedAt: time.Now(),
		closeCh:     make(chan struct{}),
	}
	s.lastHeartbeatNS.Store(time.Now().UnixNano())
	return s
}

// lastHeartbeatAt returns the time of the most recent Heartbeat (or
// session connect time if no Heartbeat has been received yet).
func (s *session) lastHeartbeatAt() time.Time {
	return time.Unix(0, s.lastHeartbeatNS.Load())
}

// noteHeartbeat updates lastHeartbeatNS. Called from
// dispatcher.handleHeartbeat. Atomic so the read path stays lock-free.
func (s *session) noteHeartbeat() {
	s.lastHeartbeatNS.Store(time.Now().UnixNano())
}

// claimedBy formats the lease holder string written into
// atlantis.jobs.claimed_by. The "dispatcher/" prefix distinguishes
// dispatched claims from direct-PG-worker claims (which use the
// PodID directly) and the session id binds the row to a specific
// worker stream so ExtendLease's claimed_by predicate stays accurate.
func (s *session) claimedBy() string {
	return "dispatcher/" + s.id
}

// inflightCount returns the current in-flight row count via an
// atomic read. Cheap enough to call from the drain loop's slot
// computation on every wake.
func (s *session) inflightCount() int {
	return int(s.inflightCounter.Load())
}

// availableSlots is how many more rows the dispatcher may push into
// this session before saturating MaxInFlight. Returns 0 when the
// session is being drained (operator drain or shutdown) so the
// drainer naturally stops dispatching to a draining worker.
func (s *session) availableSlots() int {
	if s.drained.Load() {
		return 0
	}
	free := s.maxInFlight - s.inflightCount()
	if free < 0 {
		return 0
	}
	return free
}

// handlesJob reports whether this session declared it can run jobName.
// Pre-Open-authz check; used by dispatcher.pickSession after the
// claim CTE returns rows.
func (s *session) handlesJob(jobName string) bool {
	_, ok := s.jobNames[jobName]
	return ok
}

// recordDispatch marks a row as in-flight on this session and pushes
// the Dispatch envelope into the outbox. Returns false if the outbox
// is full (which is a signal the worker is wedged — caller releases
// the row back to pending and closes the session).
//
// ackDeadline is the absolute time by which the worker MUST have
// replied with Ack; missing it means the dispatcher revokes and
// releases. Caller computes it as now() + LeaseTTLMS/2.
func (s *session) recordDispatch(d *Dispatch, leaseUntil time.Time, ackDeadline time.Time) bool {
	s.inflightMu.Lock()
	s.inflight[d.JobID] = inflightRow{
		jobID:        d.JobID,
		jobName:      d.JobName,
		dispatchedAt: time.Now(),
		leaseUntil:   leaseUntil,
		ackBy:        ackDeadline,
	}
	s.inflightMu.Unlock()
	s.inflightCounter.Add(1)
	inflightGauge.WithLabelValues(s.queue, s.id).Set(float64(s.inflightCount()))

	// Non-blocking enqueue. If the outbox is full (worker not Recv-ing
	// fast enough), it means the stream's flow control has saturated
	// — that's the dead-worker signal. Caller falls back to releasing
	// the row.
	select {
	case s.outbox <- &DispatchEnvelope{Dispatch: d}:
		s.appendEvent(sessionEvent{
			At: time.Now(), Kind: "dispatched", JobID: d.JobID, JobName: d.JobName,
		})
		return true
	default:
		// Failed to enqueue. Undo bookkeeping; caller will release.
		s.inflightMu.Lock()
		delete(s.inflight, d.JobID)
		s.inflightMu.Unlock()
		s.inflightCounter.Add(-1)
		inflightGauge.WithLabelValues(s.queue, s.id).Set(float64(s.inflightCount()))
		return false
	}
}

// recordAck flips the row's ackReceived flag. Idempotent — duplicate
// Acks (e.g. a worker that retried Ack on its own stream-write retry)
// are no-ops.
func (s *session) recordAck(jobID int64) (ok bool, dispatchLat time.Duration) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	row, exists := s.inflight[jobID]
	if !exists {
		return false, 0
	}
	if !row.ackReceived {
		row.ackReceived = true
		dispatchLat = time.Since(row.dispatchedAt)
		s.inflight[jobID] = row
	}
	return true, dispatchLat
}

// removeInflight drops a row from the in-flight map (used by Complete
// and Fail handling). Returns the dropped row so the caller can also
// gather its jobName for metrics labeling.
func (s *session) removeInflight(jobID int64) (inflightRow, bool) {
	s.inflightMu.Lock()
	row, ok := s.inflight[jobID]
	if ok {
		delete(s.inflight, jobID)
	}
	s.inflightMu.Unlock()
	if ok {
		s.inflightCounter.Add(-1)
		inflightGauge.WithLabelValues(s.queue, s.id).Set(float64(s.inflightCount()))
	}
	return row, ok
}

// snapshotInflight returns a copy of the current in-flight set. Used
// by dispatcher.unregister to know which rows to release back to
// pending when the session closes, and by GetWorkerSession admin RPC.
func (s *session) snapshotInflight() []inflightRow {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	out := make([]inflightRow, 0, len(s.inflight))
	for _, r := range s.inflight {
		out = append(out, r)
	}
	return out
}

// close signals all goroutines servicing the session to wind down.
// Idempotent. The actual lease-release happens in dispatcher.unregister
// so the global session-map state stays consistent with PG state.
func (s *session) close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
	})
}

// markDrained flips the drain flag. The drain loop sees this and
// stops pushing new dispatches; in-flight rows finish normally.
func (s *session) markDrained() {
	s.drained.Store(true)
}

// findExpiredAcks scans for in-flight rows whose Ack deadline has
// passed. Returns the job ids so the dispatcher can revoke them as
// "ack_timeout" and release back to pending — the worker is presumed
// dead despite an open stream (network partition mid-dispatch, etc.).
func (s *session) findExpiredAcks(now time.Time) []int64 {
	var out []int64
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	for id, row := range s.inflight {
		if !row.ackReceived && now.After(row.ackBy) {
			out = append(out, id)
		}
	}
	return out
}

// appendEvent records a state transition in the session's ring buffer.
// Caps to sessionEventCap so a long-lived session can't grow the
// slice without bound. Read by GetWorkerSession admin RPC (PR 3).
func (s *session) appendEvent(e sessionEvent) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if len(s.events) >= sessionEventCap {
		copy(s.events, s.events[1:])
		s.events = s.events[:len(s.events)-1]
	}
	s.events = append(s.events, e)
}

// snapshotEvents returns a copy of the current event ring for the
// admin RPC to render.
func (s *session) snapshotEvents() []sessionEvent {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	out := make([]sessionEvent, len(s.events))
	copy(out, s.events)
	return out
}

// jobNamesSlice returns the worker-declared job names. Order is
// insertion-order-stable across reads. The admin "Workers" RPCs use
// this to render the per-session detail panel; not in the hot path.
func (s *session) jobNamesSlice() []string {
	out := make([]string, 0, len(s.jobNames))
	for n := range s.jobNames {
		out = append(out, n)
	}
	return out
}
