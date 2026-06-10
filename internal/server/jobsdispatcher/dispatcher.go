// Package jobsdispatcher implements the server-side worker-poll
// scheduler: atlantis-server drains atlantis.jobs against its local PG
// and pushes work to remote workers connected over a bidi gRPC stream.
//
// The dispatcher complements (does not replace) the SDK direct-PG
// worker. Both modes use byte-identical SKIP LOCKED claim semantics
// (the shared helpers in clients/go/jobs/sql.go), so a direct-PG
// worker and a dispatched-worker session can drain the same queue
// without double-claiming. Operators pick which mode each caller's
// worker uses based on connectivity: same-VPC stays on direct-PG for
// cheapest latency; cross-network / firewalled callers use dispatched
// workers and avoid needing PG creds.
//
// Failure-mode guarantees:
//
//   - Stream death (worker crash, network partition): the session's
//     in-flight rows are released back to `pending`. The shared claim
//     CTE re-picks them on the next drain pass.
//   - Server death: in-flight rows stay `running` with the lease set.
//     When server restarts, the claim predicate picks them up via the
//     "lease expired" arm.
//   - Worker missing-Ack (claimed but not actually running): a
//     periodic sweeper revokes rows whose Ack deadline has passed
//     and releases them.
//
// Coexistence pact with the SDK Worker: both ALWAYS use
// `clients/go/jobs.ClaimRows` (or its wrapper). Any divergence in
// claim SQL would break the SKIP LOCKED invariant. New behavior goes
// in sql.go.

package jobsdispatcher

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/clients/go/jobs"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Config tunes the dispatcher. Defaults are picked to match the SDK
// Worker's defaults for behaviors that touch shared PG state — the
// HeartbeatBudget in particular should match the worker's so lease-
// expired-reclaim arrives at the same physical moment from either
// claimer's perspective.
type Config struct {
	// HeartbeatBudget is the lease duration written into claimed_until
	// on every claim. Workers must Heartbeat (or Complete/Fail) before
	// this elapses or the dispatcher revokes their row.
	HeartbeatBudget time.Duration

	// DrainInterval is the polling backstop for missed LISTEN/NOTIFY
	// signals. 1s matches the SDK worker.
	DrainInterval time.Duration

	// BatchSize caps the number of rows claimed per drain wake. Keeps
	// a single greedy pod from monopolizing the queue when a sibling
	// pod is also draining.
	BatchSize int

	// AckTimeoutMS is the ms a worker has between Dispatch send and
	// Ack receive. Missing it triggers ack-timeout revoke. Defaults
	// to HeartbeatBudget/2.
	AckTimeoutMS int

	// ShutdownBudget is how long graceful shutdown waits for in-flight
	// rows to finish before forcing release. See shutdown.go.
	ShutdownBudget time.Duration

	// PodID is the atlantis pod identifier (hostname-pid by default).
	// Embedded in claimed_by as "dispatcher/<podID>/<sessionID>".
	PodID string

	// IRLoader returns the current IR checkpoint snapshot. Called at
	// Open (authz) and at each dispatch-time defense-in-depth re-check.
	// Implementation lives in cmd/server/main.go; injected so the
	// dispatcher doesn't import the checkpoint storage layer directly.
	IRLoader func(ctx context.Context) (*dsl.IR, error)

	// AliasLoader returns the configured aliases for a caller from
	// caller_identities.aliases. Called once per session at Open;
	// the result is cached on the session for the lifetime of the
	// stream, so a steady-state worker never re-fetches. A nil
	// AliasLoader degrades to no-alias matching — equivalent to the
	// pre-aliases behavior.
	AliasLoader func(ctx context.Context, caller string) ([]string, error)

	// CallerFromContext extracts the resolved caller identity from a
	// stream context. Plumbed from cmd/server/auth.go's
	// callerFromContext so dev mode (no mTLS) and prod mode (cert CN)
	// both work without the dispatcher depending on cmd/server.
	CallerFromContext func(ctx context.Context) string

	// Logger is the dispatcher's structured log. Inherits from the
	// server's slog.
	Logger *slog.Logger
}

// DefaultConfig returns conservative defaults aligned with the SDK
// Worker. Callers can override individual fields before passing to
// New.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "atlantis"
	}
	return Config{
		HeartbeatBudget: 30 * time.Second,
		DrainInterval:   time.Second,
		BatchSize:       50,
		AckTimeoutMS:    15000, // matches HeartbeatBudget/2 default
		ShutdownBudget:  30 * time.Second,
		PodID:           host,
		Logger:          slog.Default(),
	}
}

// Dispatcher owns the drain loop per queue plus the connected-session
// registry. One Dispatcher per atlantis pod.
type Dispatcher struct {
	pool *pgxpool.Pool
	cfg  Config

	// mu guards sessions + sessionsByQueue + inflightOwner.
	// Held briefly: lookup/insert ops only, never around PG round-
	// trips or stream Send.
	mu sync.RWMutex

	// sessions maps sessionID → session. Source of truth for "what
	// workers are connected." ListConnectedWorkers reads this.
	sessions map[string]*session

	// sessionsByQueue is a denormalized view for the drain loop's
	// per-queue scan. Rebuilt on register / unregister.
	sessionsByQueue map[string][]*session

	// inflightOwner maps job_id → session so heartbeat/ack/complete/
	// fail envelopes can update the right session's bookkeeping in
	// O(1). Also used by the dispatch-time defensive lookup.
	inflightOwner map[int64]*session

	// rrCursor round-robins session selection per (queue, jobName).
	// Approximate fairness: doesn't need to be persistent across
	// restarts, just balance under steady-state load.
	rrCursor sync.Map // map[routeKey]*atomic.Uint32

	// queueRunning tracks which queues have a runQueue goroutine
	// active. Prevents two RunQueue calls for the same queue.
	queueRunning sync.Map // map[string]struct{}

	// drainStopOnce + drainStopCh signal the shutdown sweeper to stop.
	drainStopOnce sync.Once
	drainStopCh   chan struct{}
}

// New constructs a Dispatcher against the supplied pool + config. Does
// not start any goroutines; the caller starts the per-queue drain
// loops via RunQueue.
func New(pool *pgxpool.Pool, cfg Config) *Dispatcher {
	if cfg.HeartbeatBudget <= 0 {
		cfg.HeartbeatBudget = 30 * time.Second
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.AckTimeoutMS <= 0 {
		cfg.AckTimeoutMS = int(cfg.HeartbeatBudget.Milliseconds() / 2)
	}
	if cfg.ShutdownBudget <= 0 {
		cfg.ShutdownBudget = 30 * time.Second
	}
	if cfg.PodID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "atlantis"
		}
		cfg.PodID = host
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CallerFromContext == nil {
		// Defensive default: production wiring ALWAYS sets this so
		// authz can resolve a real CN. The empty-default exists so
		// tests that don't care about authz can leave it nil.
		cfg.CallerFromContext = func(context.Context) string { return "anonymous" }
	}
	return &Dispatcher{
		pool:            pool,
		cfg:             cfg,
		sessions:        make(map[string]*session),
		sessionsByQueue: make(map[string][]*session),
		inflightOwner:   make(map[int64]*session),
		drainStopCh:     make(chan struct{}),
	}
}

// RunQueue starts the drain loop for queue. Blocks until ctx is
// canceled. Safe to call once per queue; subsequent calls for the
// same queue are no-ops (so a sloppy main.go wiring doesn't start
// duplicate drainers).
func (d *Dispatcher) RunQueue(ctx context.Context, queue string) {
	if _, loaded := d.queueRunning.LoadOrStore(queue, struct{}{}); loaded {
		d.cfg.Logger.Warn("dispatcher: RunQueue called twice", "queue", queue)
		return
	}
	defer d.queueRunning.Delete(queue)
	defer func() {
		if rec := recover(); rec != nil {
			d.cfg.Logger.Error("dispatcher: queue drain panic", "queue", queue, "panic", rec)
		}
	}()

	// LISTEN/NOTIFY wake channel — buffered to 1, coalesced.
	notifyCh := make(chan struct{}, 1)
	go jobs.PgListen(ctx, d.pool, "atl_jobs", func(payload string) bool {
		return payload == queue
	}, notifyCh, d.cfg.Logger.With("dispatcher_queue", queue))

	ticker := time.NewTicker(d.cfg.DrainInterval)
	defer ticker.Stop()

	// Seed drain so any pre-LISTEN rows get picked up.
	d.drainOnce(ctx, queue)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.drainOnce(ctx, queue)
			d.sweepAckTimeouts(ctx, queue)
		case <-notifyCh:
			d.drainOnce(ctx, queue)
		}
	}
}

// drainOnce claims and routes one batch.
//
// Steps:
//
//  1. Snapshot sessions for this queue. If none, return (back-off).
//  2. Compute per-session available slots and the union of job names
//     across sessions with slots. Cap total claim by BatchSize.
//  3. Call ClaimRows with the job-name allowlist.
//  4. For each claimed row, pick a session via round-robin among
//     sessions that handle that row's jobName. If no session has
//     slots for that jobName (race: session disconnected post-claim),
//     release back to pending.
func (d *Dispatcher) drainOnce(ctx context.Context, queue string) {
	defer func() {
		if rec := recover(); rec != nil {
			d.cfg.Logger.Error("dispatcher: drainOnce panic", "queue", queue, "panic", rec)
		}
	}()

	d.mu.RLock()
	targets := append([]*session{}, d.sessionsByQueue[queue]...)
	d.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	perSessSlot := make(map[*session]int, len(targets))
	jobNameSet := make(map[string]struct{})
	for _, s := range targets {
		slot := s.availableSlots()
		if slot <= 0 {
			continue
		}
		perSessSlot[s] = slot
		for n := range s.jobNames {
			jobNameSet[n] = struct{}{}
		}
	}
	if len(perSessSlot) == 0 {
		return
	}

	jobNames := make([]string, 0, len(jobNameSet))
	for n := range jobNameSet {
		jobNames = append(jobNames, n)
	}

	total := 0
	for _, n := range perSessSlot {
		total += n
	}
	if total > d.cfg.BatchSize {
		total = d.cfg.BatchSize
	}

	leaseUntil := time.Now().Add(d.cfg.HeartbeatBudget)
	rows, err := jobs.ClaimRows(ctx, d.pool, queue, jobNames, total,
		"dispatcher/"+d.cfg.PodID, leaseUntil,
		jobs.WorkerKindDispatched, d.cfg.PodID /* placeholder; per-session id applied below */)
	if err != nil {
		d.cfg.Logger.Warn("dispatcher: claim", "queue", queue, "err", err)
		return
	}

	for _, row := range rows {
		// Dispatch-time defensive authz re-check. The IR could have
		// changed between Open and now (caller re-applied with a
		// tighter visible_to). If so, release the row and skip.
		ir, irErr := d.cfg.IRLoader(ctx)
		if irErr != nil {
			d.cfg.Logger.Warn("dispatcher: IR load for re-authz", "err", irErr)
		}

		s := d.pickSession(queue, row.JobName, perSessSlot)
		if s == nil {
			// Race: session for this jobName disconnected after we
			// computed the union. Release back to pending.
			d.releaseClaimed(ctx, row.ID, "dispatcher/"+d.cfg.PodID, "no_session_post_claim")
			revokedTotal.WithLabelValues(queue, "no_session_post_claim").Inc()
			continue
		}

		if ir != nil {
			if authzErr := CheckSingleAuthz(s.caller, s.aliases, row.JobName, ir); authzErr != nil {
				d.cfg.Logger.Info("dispatcher: authz rejected at dispatch",
					"session", s.id, "caller", s.caller,
					"job_id", row.ID, "job_name", row.JobName, "err", authzErr)
				s.appendEvent(sessionEvent{
					At: time.Now(), Kind: "authz_rejected_post_open",
					JobID: row.ID, JobName: row.JobName, Note: authzErr.Error(),
				})
				d.releaseClaimed(ctx, row.ID, s.claimedBy(), "authz_rejected")
				revokedTotal.WithLabelValues(queue, "authz_rejected").Inc()
				continue
			}
		}

		// Rewrite claimed_by + worker_session_id to bind the row to this
		// specific session. The initial Claim set claimed_by to a
		// pod-scoped placeholder so ExtendLease's predicate can guard
		// per-session ownership. We update with one row-targeted UPDATE.
		if err := d.bindClaimToSession(ctx, row.ID, s); err != nil {
			d.cfg.Logger.Warn("dispatcher: bind claim to session", "session", s.id, "row", row.ID, "err", err)
			d.releaseClaimed(ctx, row.ID, "dispatcher/"+d.cfg.PodID, "bind_failed")
			revokedTotal.WithLabelValues(queue, "bind_failed").Inc()
			continue
		}

		// Build the Dispatch envelope. Bookkeep + push to outbox.
		dispatch := buildDispatch(&row)
		ackBy := time.Now().Add(time.Duration(d.cfg.AckTimeoutMS) * time.Millisecond)
		if !s.recordDispatch(dispatch, leaseUntil, ackBy) {
			d.cfg.Logger.Warn("dispatcher: outbox full (worker likely wedged)",
				"session", s.id, "queue", queue)
			d.releaseClaimed(ctx, row.ID, s.claimedBy(), "outbox_full")
			revokedTotal.WithLabelValues(queue, "outbox_full").Inc()
			s.cntRevoked.Add(1)
			// Force session close — worker isn't Recv-ing fast enough.
			s.close()
			continue
		}
		d.trackInflight(row.ID, s)
		dispatchedTotal.WithLabelValues(queue, row.JobName).Inc()
		s.cntDispatched.Add(1)

		// Decrement budget so the next iteration uses up-to-date slots.
		perSessSlot[s]--
		if perSessSlot[s] <= 0 {
			delete(perSessSlot, s)
		}
	}
}

// pickSession returns a session that (a) handles jobName and (b) has
// at least one available slot, via approximate round-robin. Returns
// nil when no session is eligible.
func (d *Dispatcher) pickSession(queue, jobName string, budget map[*session]int) *session {
	eligible := make([]*session, 0, len(budget))
	for s, slots := range budget {
		if slots > 0 && s.handlesJob(jobName) {
			eligible = append(eligible, s)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) == 1 {
		return eligible[0]
	}
	key := routeKey{queue: queue, jobName: jobName}
	cursorAny, _ := d.rrCursor.LoadOrStore(key, newCursor())
	cursor := cursorAny.(*cursor)
	idx := cursor.next() % uint32(len(eligible))
	return eligible[idx]
}

type routeKey struct{ queue, jobName string }

// register inserts a session into the dispatcher's registry. Called
// from handleWorkerSession after authz passes.
func (d *Dispatcher) register(s *session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions[s.id] = s
	d.sessionsByQueue[s.queue] = append(d.sessionsByQueue[s.queue], s)
	sessionsActive.WithLabelValues(s.queue, s.caller).Inc()
}

// unregister removes a session and releases all its in-flight rows
// back to pending. Called when the WorkerSession stream ends (Recv
// error, ctx cancel, operator Evict).
//
// reason is recorded on the released rows (see ReleaseRow's "released:"
// prefix in clients/go/jobs/sql.go).
func (d *Dispatcher) unregister(ctx context.Context, s *session, reason string) {
	d.mu.Lock()
	delete(d.sessions, s.id)
	// Rebuild the queue list without s.
	if list, ok := d.sessionsByQueue[s.queue]; ok {
		filtered := list[:0]
		for _, x := range list {
			if x != s {
				filtered = append(filtered, x)
			}
		}
		if len(filtered) == 0 {
			delete(d.sessionsByQueue, s.queue)
		} else {
			d.sessionsByQueue[s.queue] = filtered
		}
	}
	// Drop owner pointers for everything this session was running.
	inflight := s.snapshotInflight()
	for _, row := range inflight {
		delete(d.inflightOwner, row.jobID)
	}
	d.mu.Unlock()

	sessionsActive.WithLabelValues(s.queue, s.caller).Dec()
	inflightGauge.DeleteLabelValues(s.queue, s.id)

	// Release each in-flight row back to pending so a sibling worker
	// (or this worker after reconnect) can re-claim.
	if len(inflight) > 0 {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, row := range inflight {
			if err := jobs.ReleaseRow(releaseCtx, d.pool, row.jobID, s.claimedBy(), reason); err != nil {
				d.cfg.Logger.Warn("dispatcher: release on unregister",
					"session", s.id, "row", row.jobID, "err", err)
			}
			revokedTotal.WithLabelValues(s.queue, reason).Inc()
		}
	}

	s.close()
	d.cfg.Logger.Info("dispatcher: session closed",
		"session", s.id, "caller", s.caller, "queue", s.queue,
		"inflight_released", len(inflight), "reason", reason)
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "session_closed", Note: reason,
	})
	_ = ctx // reserved for future per-call ctx; not used here
}

// trackInflight binds a job id → session in the dispatcher-level map
// so envelope handlers can find the right session in O(1).
func (d *Dispatcher) trackInflight(jobID int64, s *session) {
	d.mu.Lock()
	d.inflightOwner[jobID] = s
	d.mu.Unlock()
}

// untrackInflight is called after a row reaches a terminal state.
func (d *Dispatcher) untrackInflight(jobID int64) *session {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.inflightOwner[jobID]
	delete(d.inflightOwner, jobID)
	return s
}

// handleHeartbeat is a batched lease bump. Validates that the
// supplied job ids actually belong to this session before extending,
// so a buggy worker can't bump a peer's leases.
func (d *Dispatcher) handleHeartbeat(ctx context.Context, s *session, hb *Heartbeat) {
	if len(hb.JobIDs) == 0 {
		return
	}
	if len(hb.JobIDs) > MaxHeartbeatIDsPerFrame {
		hb.JobIDs = hb.JobIDs[:MaxHeartbeatIDsPerFrame]
	}
	owned := make([]int64, 0, len(hb.JobIDs))
	for _, id := range hb.JobIDs {
		s.inflightMu.Lock()
		_, ok := s.inflight[id]
		s.inflightMu.Unlock()
		if ok {
			owned = append(owned, id)
		}
	}
	if len(owned) == 0 {
		return
	}
	if err := jobs.ExtendLease(ctx, d.pool, owned, s.claimedBy(), d.cfg.HeartbeatBudget); err != nil {
		d.cfg.Logger.Warn("dispatcher: heartbeat extend", "session", s.id, "err", err)
		return
	}
	s.noteHeartbeat()
	// Also bump the in-memory ackBy clock so the sweeper doesn't
	// revoke a row whose worker IS alive (Heartbeat without Ack is a
	// valid state for jobs the worker has fully accepted but hasn't
	// signaled Ack on, e.g. paused for handler-side resource).
	now := time.Now()
	newAckBy := now.Add(time.Duration(d.cfg.AckTimeoutMS) * time.Millisecond)
	newLease := now.Add(d.cfg.HeartbeatBudget)
	s.inflightMu.Lock()
	for _, id := range owned {
		row := s.inflight[id]
		row.leaseUntil = newLease
		if !row.ackReceived {
			row.ackBy = newAckBy
		}
		s.inflight[id] = row
	}
	s.inflightMu.Unlock()
	s.appendEvent(sessionEvent{
		At: now, Kind: "heartbeat_received", Note: "bumped_leases",
	})
}

// handleAck records the worker's acknowledgement of a Dispatch.
func (d *Dispatcher) handleAck(s *session, a *Ack) {
	ok, lat := s.recordAck(a.JobID)
	if !ok {
		// Ack for a job we don't know about — likely a stale Ack from
		// a row we already revoked. Ignore.
		return
	}
	if lat > 0 {
		dispatchLatency.WithLabelValues(s.queue).Observe(lat.Seconds())
	}
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "acked", JobID: a.JobID,
	})
}

// handleComplete writes the terminal success state.
func (d *Dispatcher) handleComplete(ctx context.Context, s *session, c *Complete) {
	row, ok := s.removeInflight(c.JobID)
	if !ok {
		d.cfg.Logger.Info("dispatcher: complete for unknown row",
			"session", s.id, "job_id", c.JobID)
		return
	}
	d.untrackInflight(c.JobID)
	if err := jobs.MarkComplete(ctx, d.pool, c.JobID); err != nil {
		d.cfg.Logger.Warn("dispatcher: markComplete",
			"session", s.id, "row", c.JobID, "err", err)
	}
	completedTotal.WithLabelValues(s.queue, row.jobName).Inc()
	s.cntCompleted.Add(1)
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "completed", JobID: c.JobID, JobName: row.jobName,
	})
}

// handleFail writes the terminal-or-retry failure state via the
// shared ReportFailure helper.
func (d *Dispatcher) handleFail(ctx context.Context, s *session, f *Fail) {
	row, ok := s.removeInflight(f.JobID)
	if !ok {
		d.cfg.Logger.Info("dispatcher: fail for unknown row",
			"session", s.id, "job_id", f.JobID)
		return
	}
	d.untrackInflight(f.JobID)

	// Resolve current attempts + max_retries from PG so the retry-
	// vs-DLQ decision uses authoritative state (the row's attempts
	// was incremented at claim; the worker's view could be stale if
	// it cached the Dispatch envelope's snapshot).
	attempts, maxRetries, lookupErr := d.lookupAttempts(ctx, f.JobID)
	if lookupErr != nil {
		d.cfg.Logger.Warn("dispatcher: lookup attempts on fail",
			"session", s.id, "row", f.JobID, "err", lookupErr)
		// Fall back to in-memory snapshot — better than dropping the
		// terminal write.
		attempts = 1
		maxRetries = 0
	}

	// Retry=false from the worker means operator-intent DLQ.
	if !f.Retry {
		if err := jobs.MoveToDLQ(ctx, d.pool, f.JobID, f.Error); err != nil {
			d.cfg.Logger.Warn("dispatcher: moveToDLQ on fail",
				"session", s.id, "row", f.JobID, "err", err)
		}
		failedTotal.WithLabelValues(s.queue, row.jobName, "true").Inc()
		s.cntFailed.Add(1)
		s.appendEvent(sessionEvent{
			At: time.Now(), Kind: "failed", JobID: f.JobID, JobName: row.jobName, Note: "dlq",
		})
		return
	}
	terminal := attempts >= maxRetries
	if err := jobs.ReportFailure(ctx, d.pool, f.JobID, attempts, maxRetries, f.Error); err != nil {
		d.cfg.Logger.Warn("dispatcher: reportFailure",
			"session", s.id, "row", f.JobID, "err", err)
	}
	label := "false"
	if terminal {
		label = "true"
	}
	failedTotal.WithLabelValues(s.queue, row.jobName, label).Inc()
	s.cntFailed.Add(1)
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "failed", JobID: f.JobID, JobName: row.jobName,
		Note: f.Error,
	})
}

// sweepAckTimeouts revokes rows whose Ack deadline has passed.
func (d *Dispatcher) sweepAckTimeouts(ctx context.Context, queue string) {
	d.mu.RLock()
	targets := append([]*session{}, d.sessionsByQueue[queue]...)
	d.mu.RUnlock()

	now := time.Now()
	for _, s := range targets {
		expired := s.findExpiredAcks(now)
		for _, jobID := range expired {
			if _, ok := s.removeInflight(jobID); !ok {
				continue
			}
			d.untrackInflight(jobID)
			// Push Revoke to worker (best-effort) and release row.
			select {
			case s.outbox <- &DispatchEnvelope{Revoke: &Revoke{JobID: jobID, Reason: "ack_timeout"}}:
			default:
			}
			if err := jobs.ReleaseRow(ctx, d.pool, jobID, s.claimedBy(), "ack_timeout"); err != nil {
				d.cfg.Logger.Warn("dispatcher: release on ack timeout",
					"session", s.id, "row", jobID, "err", err)
			}
			revokedTotal.WithLabelValues(queue, "ack_timeout").Inc()
			s.cntRevoked.Add(1)
			s.appendEvent(sessionEvent{
				At: now, Kind: "revoked", JobID: jobID, Note: "ack_timeout",
			})
		}
	}
}

// releaseClaimed wraps jobs.ReleaseRow with logging suitable for the
// dispatcher's claim-loop paths (no_session_post_claim, bind_failed,
// outbox_full, authz_rejected).
func (d *Dispatcher) releaseClaimed(ctx context.Context, jobID int64, claimedBy, reason string) {
	if err := jobs.ReleaseRow(ctx, d.pool, jobID, claimedBy, reason); err != nil {
		d.cfg.Logger.Warn("dispatcher: release claimed",
			"row", jobID, "reason", reason, "err", err)
	}
}

// bindClaimToSession rewrites claimed_by and worker_session_id on a
// row freshly claimed by the queue-scoped placeholder. After this,
// ExtendLease's `claimed_by = $3` predicate matches only this session.
func (d *Dispatcher) bindClaimToSession(ctx context.Context, jobID int64, s *session) error {
	_, err := d.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET claimed_by        = $2,
       worker_session_id = $3
 WHERE id = $1`, jobID, s.claimedBy(), s.id)
	return err
}

// lookupAttempts re-reads attempts + max_retries for a row. Used by
// handleFail to make the retry-vs-DLQ decision against authoritative
// PG state rather than a possibly-stale Dispatch snapshot.
func (d *Dispatcher) lookupAttempts(ctx context.Context, jobID int64) (int, int, error) {
	var attempts, maxRetries int
	row := d.pool.QueryRow(ctx, `SELECT attempts, max_retries FROM atlantis.jobs WHERE id = $1`, jobID)
	if err := row.Scan(&attempts, &maxRetries); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, 0, err
		}
		return 0, 0, err
	}
	return attempts, maxRetries, nil
}

// buildDispatch converts a ClaimedRow to a Dispatch envelope.
func buildDispatch(r *jobs.ClaimedRow) *Dispatch {
	scheduledFor := ""
	if !r.ScheduledFor.IsZero() {
		scheduledFor = r.ScheduledFor.UTC().Format(time.RFC3339)
	}
	enqueuedAt := ""
	if !r.EnqueuedAt.IsZero() {
		enqueuedAt = r.EnqueuedAt.UTC().Format(time.RFC3339)
	}
	return &Dispatch{
		JobID:        r.ID,
		JobName:      r.JobName,
		Args:         r.Args,
		Attempts:     r.Attempts,
		MaxRetries:   r.MaxRetries,
		TimeoutMS:    r.TimeoutMS,
		ScheduledFor: scheduledFor,
		EnqueuedAt:   enqueuedAt,
		TraceCtx:     r.TraceCtx,
	}
}
