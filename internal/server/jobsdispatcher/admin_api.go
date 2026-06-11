// Public observation + control surface for the admin RPCs that the
// console "Workers" tab calls.
//
// The dispatcher's in-memory state is the source of truth for "what
// workers are connected." These methods expose snapshots of that
// state without exposing the internal session/pointer types. The
// SessionSnapshot / SessionDetail / EventSnapshot DTOs are stable
// wire shapes; renaming a session struct field doesn't break the
// console contract.
//
// Drain and Evict are operator-initiated lifecycle controls:
//
//   - Drain: stop dispatching new rows to this session; let in-flight
//     finish; close the stream cleanly once inflight count = 0. The
//     graceful path. Console polls until the session disappears from
//     the list.
//   - Evict: send Goodbye, force-close the stream, release every
//     in-flight row back to pending immediately. The destructive
//     path for a wedged/malicious worker.
//
// Both are admin+sudo-gated at the BFF layer (internal/console).
// This package trusts its caller — the dispatcher's only authz
// check is the per-stream visible_to gate, not the per-RPC gate.

package jobsdispatcher

import (
	"context"
	"fmt"
	"time"
)

// SessionSnapshot is the per-row payload of ListConnectedWorkers.
// Counters are point-in-time reads — no per-second rate is computed
// here; the console derives rates from successive polls.
type SessionSnapshot struct {
	SessionID       string    `json:"session_id"`
	Caller          string    `json:"caller"`
	Queue           string    `json:"queue"`
	PodID           string    `json:"pod_id,omitempty"`
	SDKVersion      string    `json:"sdk_version,omitempty"`
	ConnectedAt     time.Time `json:"connected_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	MaxInFlight     int       `json:"max_in_flight"`
	InflightCount   int       `json:"inflight_count"`
	Dispatched      int64     `json:"dispatched"`
	Completed       int64     `json:"completed"`
	Failed          int64     `json:"failed"`
	Revoked         int64     `json:"revoked"`
	Drained         bool      `json:"drained,omitempty"`
}

// SessionDetail extends SessionSnapshot with everything the per-session
// drill-in page renders: declared job names, currently in-flight rows,
// and the recent-events ring.
type SessionDetail struct {
	SessionSnapshot
	JobNames []string         `json:"job_names"`
	Inflight []InflightDetail `json:"inflight"`
	Events   []EventSnapshot  `json:"events"`
}

// InflightDetail is a single in-flight row the worker is running. The
// console links job_id → existing Jobs detail page.
type InflightDetail struct {
	JobID        int64     `json:"job_id"`
	JobName      string    `json:"job_name"`
	DispatchedAt time.Time `json:"dispatched_at"`
	AckReceived  bool      `json:"ack_received"`
}

// EventSnapshot is one entry from the session's ring-buffer log.
type EventSnapshot struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	JobID   int64     `json:"job_id,omitempty"`
	JobName string    `json:"job_name,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// SnapshotSessions returns every currently-connected session. Reads
// in-memory state under d.mu.RLock so it's safe to call concurrently
// with register / unregister / drain.
//
// Ordering: stable-by-SessionID so successive polls of the same set
// render with the same row order. Sorting in the console is per-
// column user-driven; this just gives us a consistent baseline.
func (d *Dispatcher) SnapshotSessions() []SessionSnapshot {
	d.mu.RLock()
	out := make([]SessionSnapshot, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, sessionToSnapshot(s))
	}
	d.mu.RUnlock()
	sortSnapshots(out)
	return out
}

// GetSession returns the detail for one connected session. Returns
// false when no session with that id exists (already disconnected,
// or never existed).
func (d *Dispatcher) GetSession(sessionID string) (SessionDetail, bool) {
	d.mu.RLock()
	s, ok := d.sessions[sessionID]
	d.mu.RUnlock()
	if !ok {
		return SessionDetail{}, false
	}
	detail := SessionDetail{SessionSnapshot: sessionToSnapshot(s)}
	// Initialize as empty (not nil) slices so they marshal to JSON [] and
	// not null — the console SPA reads .length on each and a null crashes
	// the session-detail page once a session has zero in-flight rows.
	detail.JobNames = s.jobNamesSlice()
	if detail.JobNames == nil {
		detail.JobNames = []string{}
	}
	detail.Inflight = []InflightDetail{}
	detail.Events = []EventSnapshot{}
	for _, row := range s.snapshotInflight() {
		detail.Inflight = append(detail.Inflight, InflightDetail{
			JobID:        row.jobID,
			JobName:      row.jobName,
			DispatchedAt: row.dispatchedAt,
			AckReceived:  row.ackReceived,
		})
	}
	for _, e := range s.snapshotEvents() {
		detail.Events = append(detail.Events, EventSnapshot{
			At:      e.At,
			Kind:    e.Kind,
			JobID:   e.JobID,
			JobName: e.JobName,
			Note:    e.Note,
		})
	}
	return detail, true
}

// DrainSession marks the session for graceful drain. The drain loop
// stops dispatching to it; in-flight rows finish normally; the
// session closes once inflight = 0. Idempotent — calling on an
// already-drained session is a no-op.
//
// Returns ErrSessionNotFound when no session matches sessionID.
// Returns nil on a successful drain initiation (the actual close
// happens asynchronously when inflight reaches zero).
func (d *Dispatcher) DrainSession(sessionID string) error {
	d.mu.RLock()
	s, ok := d.sessions[sessionID]
	d.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	s.markDrained()
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "drain_requested", Note: "operator",
	})
	d.cfg.Logger.Info("dispatcher: session drain requested",
		"session", sessionID, "caller", s.caller, "queue", s.queue,
		"inflight", s.inflightCount())

	// Background watcher: once inflight reaches zero, send Goodbye and
	// close the stream so the worker disconnects cleanly. We don't
	// block the caller — drain is intentionally async so the console
	// can poll for completion.
	go d.awaitDrainAndClose(s)
	return nil
}

// EvictSession is the destructive sibling of DrainSession: stop
// dispatching immediately, force-release every in-flight row back to
// pending, and close the stream. Use for stuck workers that don't
// respond to Drain within a reasonable budget.
//
// Returns ErrSessionNotFound when no session matches sessionID.
func (d *Dispatcher) EvictSession(sessionID string) error {
	d.mu.RLock()
	s, ok := d.sessions[sessionID]
	d.mu.RUnlock()
	if !ok {
		return ErrSessionNotFound
	}
	s.markDrained()
	// Best-effort Goodbye + Revoke for every in-flight row.
	select {
	case s.outbox <- &DispatchEnvelope{Goodbye: &Goodbye{Reason: "evicted"}}:
	default:
	}
	for _, row := range s.snapshotInflight() {
		select {
		case s.outbox <- &DispatchEnvelope{Revoke: &Revoke{JobID: row.jobID, Reason: "evicted"}}:
		default:
		}
	}
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "evicted", Note: "operator",
	})
	d.cfg.Logger.Warn("dispatcher: session evicted",
		"session", sessionID, "caller", s.caller, "queue", s.queue,
		"inflight_released", s.inflightCount())

	// Close the stream. The recv loop in WorkerSession returns, defer
	// fires unregister, which releases the rows.
	s.close()

	// Synchronously release rows so an operator-visible "in-flight ->
	// pending" transition happens before the RPC returns. The async
	// release via unregister still fires; ReleaseRow is idempotent
	// (claimed_by guard means a second call against an already-
	// released row is a no-op).
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	released := 0
	for _, row := range s.snapshotInflight() {
		if err := releaseEvicted(releaseCtx, d, s, row.jobID); err == nil {
			released++
		}
	}
	if released > 0 {
		d.cfg.Logger.Info("dispatcher: evict released rows",
			"session", sessionID, "released", released)
	}
	return nil
}

// awaitDrainAndClose polls inflight until zero (or session closes for
// another reason) and then closes the stream. Caps the wait at a
// generous budget so a stuck handler doesn't leave a drained-flagged
// session hanging forever; after the budget, falls through to a soft
// close that lets the existing release-on-disconnect path handle the
// leftover rows.
func (d *Dispatcher) awaitDrainAndClose(s *session) {
	const drainCap = 10 * time.Minute
	deadline := time.Now().Add(drainCap)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.inflightCount() == 0 {
			break
		}
		if time.Now().After(deadline) {
			d.cfg.Logger.Warn("dispatcher: drain cap exceeded, force-closing",
				"session", s.id, "inflight", s.inflightCount())
			break
		}
		select {
		case <-s.closeCh:
			return
		case <-tick.C:
		}
	}
	select {
	case s.outbox <- &DispatchEnvelope{Goodbye: &Goodbye{Reason: "drained"}}:
	default:
	}
	// Small sleep so the Goodbye actually leaves the outbox before we
	// close the channel out from under the sender goroutine.
	time.Sleep(50 * time.Millisecond)
	s.close()
}

// sessionToSnapshot snapshots the atomics + immutable fields. Called
// under d.mu.RLock (SnapshotSessions) or after a lookup (GetSession).
// Reads atomic counters without further locking.
func sessionToSnapshot(s *session) SessionSnapshot {
	return SessionSnapshot{
		SessionID:       s.id,
		Caller:          s.caller,
		Queue:           s.queue,
		PodID:           s.podID,
		SDKVersion:      s.version,
		ConnectedAt:     s.connectedAt,
		LastHeartbeatAt: s.lastHeartbeatAt(),
		MaxInFlight:     s.maxInFlight,
		InflightCount:   s.inflightCount(),
		Dispatched:      s.cntDispatched.Load(),
		Completed:       s.cntCompleted.Load(),
		Failed:          s.cntFailed.Load(),
		Revoked:         s.cntRevoked.Load(),
		Drained:         s.drained.Load(),
	}
}

// sortSnapshots sorts by SessionID for stable list rendering.
func sortSnapshots(in []SessionSnapshot) {
	// Avoid pulling in `sort` for a small slice; insertion sort.
	for i := 1; i < len(in); i++ {
		x := in[i]
		j := i - 1
		for j >= 0 && in[j].SessionID > x.SessionID {
			in[j+1] = in[j]
			j--
		}
		in[j+1] = x
	}
}

// releaseEvicted is the eviction-path release helper. Wraps
// jobs.ReleaseRow with the dispatcher's reason string so the row's
// last_error column shows "released:evicted" in PG.
func releaseEvicted(ctx context.Context, d *Dispatcher, s *session, jobID int64) error {
	// We import jobs lazily here to avoid a cycle; the file-level
	// imports above stay narrow. (The jobs package is already linked
	// elsewhere in this binary, so this isn't a heavyweight import.)
	return releaseRowFunc(ctx, d.pool, jobID, s.claimedBy(), "evicted")
}

// releaseRowFunc is a package-private indirection so tests can stub the
// release call. Production always points at jobs.ReleaseRow; tests
// can swap it to assert release was attempted. Captured at package
// init in admin_api_init.go to avoid an import cycle in this file.
var releaseRowFunc = defaultReleaseRow

// ErrSessionNotFound is returned by DrainSession / EvictSession when
// the supplied sessionID doesn't match any connected session.
var ErrSessionNotFound = fmt.Errorf("worker session not found")
