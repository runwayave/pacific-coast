// Dispatched-worker SDK. The caller-facing constructor for the
// Temporal-style worker-poll mode: connect outbound to atlantis-server
// over gRPC + mTLS, receive jobs as Dispatch envelopes, run them
// through the same Handler interface as the direct-PG Worker.
//
// Use this when:
//
//   - You don't have direct PG access to atlantis (cross-network,
//     firewalled, customer network, laptop dev against prod).
//   - You want to scale workers independently of atlantis pods.
//   - Multiple workers can share one outbound gRPC pipe instead of
//     each holding their own PG connection.
//
// Keep using NewWorker (direct-PG) when:
//
//   - Worker and atlantis are co-located (same VPC).
//   - PG access is cheap.
//   - You want the lowest possible drain latency (one fewer hop).
//
// Handler interface is unchanged: jobs.Handler.Handle(ctx, argsJSON).
// Existing handler code compiles against NewDispatchedWorker with
// zero changes.

package jobs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ServerConfig tunes the dispatched-worker SDK. Defaults are chosen
// to be safe for the typical "laptop talks to prod atlantis" path.
type ServerConfig struct {
	// MaxInFlight caps concurrent handler executions per worker.
	// Server-side clamped to [1, 256]; below 1 defaults to 16.
	MaxInFlight int

	// PodID is informational metadata sent in Open and shown in
	// atlantis logs. Defaults to hostname.
	PodID string

	// Version is the SDK build identifier. Forward-compat bookkeeping;
	// leave blank to send the empty string.
	Version string

	// Logger receives structured records of every state transition.
	Logger *slog.Logger

	// ShutdownBudget bounds how long Run waits for in-flight handlers
	// to finish after the ctx cancels. 30s default.
	ShutdownBudget time.Duration
}

// DefaultServerConfig returns sensible defaults. Mirrors the
// DefaultConfig pattern of NewWorker.
func DefaultServerConfig() ServerConfig {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return ServerConfig{
		MaxInFlight:    16,
		PodID:          host,
		Logger:         slog.Default(),
		ShutdownBudget: 30 * time.Second,
	}
}

// DispatchedWorker receives jobs from atlantis-server over a bidi
// gRPC stream and runs them via the supplied Registry. One worker per
// (queue, gRPC connection) pair. Multiple workers from the same
// process can share the same `*grpc.ClientConn`.
type DispatchedWorker struct {
	conn     *grpc.ClientConn
	registry *Registry
	queue    string
	cfg      ServerConfig

	// inflight tracks handler goroutines so Run can wait for them on
	// graceful shutdown.
	inflightMu  sync.Mutex
	inflight    map[int64]context.CancelFunc
	inflightWG  sync.WaitGroup
	inflightCnt atomic.Int32

	// dataCh and ctrlCh split the outbound envelope stream into a
	// data plane (Ack / Complete / Fail — terminal job state) and a
	// control plane (Heartbeat / Checkpoint — liveness + progress).
	//
	// runSender drains ctrlCh with priority via a two-stage select so
	// data-plane backpressure can't drown out liveness signals. Before
	// this split, when many handlers blocked simultaneously the
	// single shared channel would fill with terminal envelopes and
	// the auto-heartbeat goroutine's payloads would be silently
	// dropped by the non-blocking default: clause — server saw stale
	// heartbeats, leases expired, dispatcher revoked + re-dispatched.
	// Producing the loop we observed on 2026-06-10.
	dataCh chan *WorkerEnvelope
	ctrlCh chan *WorkerEnvelope

	// Bookkeeping captured from SessionAccepted.
	heartbeatInterval atomic.Int64 // ns
	leaseTTL          atomic.Int64 // ns
}

// NewDispatchedWorker constructs a worker bound to the given gRPC
// connection, registry, queue, and config. Does not connect; the
// stream is opened in Run.
func NewDispatchedWorker(conn *grpc.ClientConn, registry *Registry, queue string, cfg ServerConfig) *DispatchedWorker {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 16
	}
	if cfg.PodID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "worker"
		}
		cfg.PodID = host
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownBudget <= 0 {
		cfg.ShutdownBudget = 30 * time.Second
	}
	return &DispatchedWorker{
		conn:     conn,
		registry: registry,
		queue:    queue,
		cfg:      cfg,
		inflight: make(map[int64]context.CancelFunc),
		// ctrlCh is small but bursty (one heartbeat per HeartbeatMS,
		// plus one Checkpoint per Checkpoint call). Sized for ~30s of
		// queueing at typical cadence.
		ctrlCh: make(chan *WorkerEnvelope, cfg.MaxInFlight+8),
		// dataCh sees one Ack per Dispatch and one Complete/Fail per
		// terminal — bounded by inflight, sized at 2x for headroom.
		dataCh: make(chan *WorkerEnvelope, cfg.MaxInFlight*2),
	}
}

// Run opens the WorkerSession stream and drives the worker until ctx
// cancels. Reconnects on stream error with exponential backoff +
// jitter (1s base, ×2, capped 30s, ±25% jitter). Each reconnect
// re-sends Open, so a transient network blip doesn't leave the worker
// in a half-attached state.
func (w *DispatchedWorker) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := w.runOnce(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		// On clean ctx-cancel during runOnce, exit; otherwise log + back off.
		w.cfg.Logger.Warn("dispatched worker: stream ended",
			"queue", w.queue, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce opens one stream and drives it to completion (worker death,
// server disconnect, or ctx cancel). Reset of the backoff counter
// after a successful SessionAccepted is delegated to Run.
func (w *DispatchedWorker) runOnce(ctx context.Context) error {
	stream, err := w.openStream(ctx)
	if err != nil {
		return err
	}

	// Send Open.
	jobNames := w.registry.RegisteredIDs()
	if len(jobNames) > MaxJobNamesPerOpen {
		jobNames = jobNames[:MaxJobNamesPerOpen]
	}
	openEnv := &WorkerEnvelope{Open: &OpenSession{
		Queue:       w.queue,
		JobNames:    jobNames,
		MaxInFlight: w.cfg.MaxInFlight,
		PodID:       w.cfg.PodID,
		Version:     w.cfg.Version,
	}}
	if err := sendOnStream(stream, openEnv); err != nil {
		return fmt.Errorf("send Open: %w", err)
	}

	// Recv SessionAccepted.
	first, err := recvOnStream(stream)
	if err != nil {
		return fmt.Errorf("recv SessionAccepted: %w", err)
	}
	if first.SessionAccepted == nil {
		return fmt.Errorf("expected SessionAccepted, got envelope without it")
	}
	w.heartbeatInterval.Store(int64(time.Duration(first.SessionAccepted.HeartbeatMS) * time.Millisecond))
	w.leaseTTL.Store(int64(time.Duration(first.SessionAccepted.LeaseTTLMS) * time.Millisecond))
	w.cfg.Logger.Info("dispatched worker: session opened",
		"queue", w.queue,
		"session", first.SessionAccepted.SessionID,
		"heartbeat_ms", first.SessionAccepted.HeartbeatMS)

	// Sender + heartbeat + recv goroutines.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.runSender(runCtx, stream)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.runHeartbeat(runCtx)
	}()

	recvErr := w.runRecv(runCtx, stream)

	// Graceful shutdown of in-flight handlers if ctx cancelled.
	if ctx.Err() != nil {
		w.gracefulDrain(ctx)
	}

	cancelRun()
	wg.Wait()
	return recvErr
}

// openStream initiates the bidi stream with JSON-envelope codec.
func (w *DispatchedWorker) openStream(ctx context.Context) (grpc.ClientStream, error) {
	streamDesc := &grpc.StreamDesc{
		StreamName:    "WorkerSession",
		ServerStreams: true,
		ClientStreams: true,
	}
	// Force the JSON codec so the request/response shapes use our
	// jsonMsg envelope rather than gRPC's default proto codec.
	stream, err := w.conn.NewStream(
		ctx,
		streamDesc,
		"/atlantis.workerdispatch.v1.WorkerDispatch/WorkerSession",
		grpc.ForceCodecV2(dispatchedJSONCodec{}),
	)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	// Attach a defensive metadata stamp so the server can correlate
	// reconnect storms to a specific worker pod.
	_ = stream.Header // exhaust the header chan so SendMsg doesn't deadlock on early failures
	_ = metadata.Pairs("x-worker-pod", w.cfg.PodID)
	return stream, nil
}

// runRecv reads Dispatch / Revoke / Goodbye envelopes and routes them.
func (w *DispatchedWorker) runRecv(ctx context.Context, stream grpc.ClientStream) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		env, err := recvOnStream(stream)
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
				return nil
			}
			return err
		}
		switch {
		case env.Dispatch != nil:
			w.handleDispatch(ctx, env.Dispatch)
		case env.Revoke != nil:
			w.handleRevoke(env.Revoke)
		case env.Goodbye != nil:
			w.cfg.Logger.Info("dispatched worker: server goodbye",
				"queue", w.queue, "reason", env.Goodbye.Reason)
			return nil
		case env.SessionAccepted != nil:
			// Duplicate SessionAccepted after reconnect — server should
			// not send this; ignore.
		default:
			w.cfg.Logger.Warn("dispatched worker: empty envelope received")
		}
	}
}

// streamCheckpointer is the DispatchedWorker's Checkpointer
// implementation. Where the direct-PG Worker's pgCheckpointer hits
// the database from its own pool, this one emits a CheckpointMsg
// envelope over the gRPC control channel — atlantis's dispatcher
// receives it and persists the progress columns on the worker's
// behalf. Handler code is portable: jobs.Checkpoint(ctx, pct, msg)
// works the same way regardless of which Worker is hosting it.
type streamCheckpointer struct {
	w     *DispatchedWorker
	jobID int64
}

func newStreamCheckpointer(w *DispatchedWorker, jobID int64) Checkpointer {
	return &streamCheckpointer{w: w, jobID: jobID}
}

// Report sends a CheckpointMsg via the priority control channel.
// Clamps pct to [0, 100] and truncates msg to MaxCheckpointMsgChars
// before sending so the server's identical defensive limits never
// have to actually reject a payload from a well-behaved SDK.
func (c *streamCheckpointer) Report(_ context.Context, pct int, msg string) error {
	if c == nil || c.w == nil {
		return errors.New("jobs: nil stream checkpointer")
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if len(msg) > MaxCheckpointMsgChars {
		msg = msg[:MaxCheckpointMsgChars]
	}
	c.w.enqueueCtrl(&WorkerEnvelope{Checkpoint: &CheckpointMsg{
		JobID: c.jobID,
		Pct:   pct,
		Msg:   msg,
	}})
	return nil
}

// MaxCheckpointMsgChars mirrors the server-side cap in
// internal/server/jobsdispatcher/proto.go. Sync these constants.
const MaxCheckpointMsgChars = 256

// handleDispatch spins a handler goroutine for one job. Sends Ack
// immediately, installs a stream checkpointer in the handler ctx so
// jobs.Checkpoint(ctx, pct, msg) routes through the stream, then runs
// the handler, then sends Complete or Fail.
func (w *DispatchedWorker) handleDispatch(ctx context.Context, d *Dispatch) {
	handler := w.registry.Lookup(d.JobName)
	if handler == nil {
		// No handler — treat as transient fail. Worker doesn't know
		// whether a sibling worker on a different pod has the handler;
		// surface it and let atlantis re-route.
		w.enqueueData(&WorkerEnvelope{Fail: &Fail{
			JobID: d.JobID,
			Error: fmt.Sprintf("handler %q not registered", d.JobName),
			Retry: true,
		}})
		return
	}

	// Ack first — atlantis's missing-Ack sweeper revokes if we don't.
	w.enqueueData(&WorkerEnvelope{Ack: &Ack{JobID: d.JobID}})

	handlerCtx, cancel := context.WithCancel(ctx)
	if d.TimeoutMS > 0 {
		handlerCtx, cancel = context.WithTimeout(ctx, time.Duration(d.TimeoutMS)*time.Millisecond)
	}
	// Install the stream checkpointer so the handler's jobs.Checkpoint
	// calls route through the priority ctrl channel. Same interface
	// the direct-PG worker installs — handlers don't know which
	// backend is running them.
	handlerCtx = withCheckpointer(handlerCtx, newStreamCheckpointer(w, d.JobID))
	w.inflightMu.Lock()
	w.inflight[d.JobID] = cancel
	w.inflightMu.Unlock()
	w.inflightCnt.Add(1)
	w.inflightWG.Add(1)

	go func() {
		defer w.inflightWG.Done()
		defer w.inflightCnt.Add(-1)
		defer w.untrackInflight(d.JobID)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				w.cfg.Logger.Error("dispatched worker: handler panic",
					"job_name", d.JobName, "job_id", d.JobID, "panic", rec)
				w.enqueueData(&WorkerEnvelope{Fail: &Fail{
					JobID: d.JobID,
					Error: fmt.Sprintf("handler panic: %v", rec),
					Retry: true,
				}})
			}
		}()
		err := handler.Handle(handlerCtx, d.Args)
		if err == nil {
			w.enqueueData(&WorkerEnvelope{Complete: &Complete{JobID: d.JobID}})
			return
		}
		w.enqueueData(&WorkerEnvelope{Fail: &Fail{
			JobID: d.JobID,
			Error: err.Error(),
			Retry: true,
		}})
	}()
}

// handleRevoke cancels the in-flight handler for a job atlantis has
// pulled back (lease expired, operator action, timeout).
func (w *DispatchedWorker) handleRevoke(r *Revoke) {
	w.inflightMu.Lock()
	cancel := w.inflight[r.JobID]
	delete(w.inflight, r.JobID)
	w.inflightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *DispatchedWorker) untrackInflight(jobID int64) {
	w.inflightMu.Lock()
	delete(w.inflight, jobID)
	w.inflightMu.Unlock()
}

// runHeartbeat sends Heartbeat envelopes at the server-advised
// cadence. Captures every job id currently in inflight in a single
// envelope.
func (w *DispatchedWorker) runHeartbeat(ctx context.Context) {
	interval := time.Duration(w.heartbeatInterval.Load())
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids := w.snapshotInflightIDs()
			if len(ids) == 0 {
				continue
			}
			w.enqueueCtrl(&WorkerEnvelope{Heartbeat: &Heartbeat{JobIDs: ids}})
		}
	}
}

func (w *DispatchedWorker) snapshotInflightIDs() []int64 {
	w.inflightMu.Lock()
	defer w.inflightMu.Unlock()
	out := make([]int64, 0, len(w.inflight))
	for id := range w.inflight {
		out = append(out, id)
	}
	return out
}

// runSender pulls envelopes from ctrlCh (with priority) and dataCh
// and writes them to the stream. Serializing through one goroutine
// eliminates concurrent SendMsg calls; the priority-select on
// ctrlCh ensures liveness signals (Heartbeat/Checkpoint) flush
// before terminal envelopes (Ack/Complete/Fail) when both are
// pending. This is the structural fix for the stale-heartbeat bug.
func (w *DispatchedWorker) runSender(ctx context.Context, stream grpc.ClientStream) {
	send := func(env *WorkerEnvelope) bool {
		if err := sendOnStream(stream, env); err != nil {
			w.cfg.Logger.Warn("dispatched worker: send", "err", err)
			return false
		}
		return true
	}
	for {
		// Stage 1: ctx + ctrlCh only. If a control envelope is
		// ready, dispatch it before reading from dataCh — heartbeats
		// can't be drowned out by terminal-envelope volume.
		select {
		case <-ctx.Done():
			return
		case env := <-w.ctrlCh:
			if !send(env) {
				return
			}
			continue
		default:
		}
		// Stage 2: nothing on ctrlCh right now; read from either.
		select {
		case <-ctx.Done():
			return
		case env := <-w.ctrlCh:
			if !send(env) {
				return
			}
		case env := <-w.dataCh:
			if !send(env) {
				return
			}
		}
	}
}

// enqueueCtrl pushes liveness envelopes (Heartbeat / Checkpoint).
// runSender drains this channel with priority so a saturated data
// plane can't drop heartbeats.
func (w *DispatchedWorker) enqueueCtrl(env *WorkerEnvelope) {
	select {
	case w.ctrlCh <- env:
	default:
		// Should be exceedingly rare: ctrlCh is sized for the typical
		// burst window. If we ever see this log in prod, bump the
		// capacity at construction; don't drop heartbeats silently.
		w.cfg.Logger.Error("dispatched worker: ctrl channel full, dropping",
			"variant", envelopeVariant(env))
	}
}

// enqueueData pushes terminal envelopes (Ack / Complete / Fail).
// Bounded by inflight cap; the only way to fill it is genuine server
// backpressure, at which point dropping new envelopes is the right
// thing — atlantis will re-dispatch the row on the next claim.
func (w *DispatchedWorker) enqueueData(env *WorkerEnvelope) {
	select {
	case w.dataCh <- env:
	default:
		w.cfg.Logger.Error("dispatched worker: data channel full, dropping",
			"variant", envelopeVariant(env))
	}
}

func envelopeVariant(env *WorkerEnvelope) string {
	switch {
	case env.Open != nil:
		return "Open"
	case env.Heartbeat != nil:
		return "Heartbeat"
	case env.Ack != nil:
		return "Ack"
	case env.Complete != nil:
		return "Complete"
	case env.Fail != nil:
		return "Fail"
	}
	return "unknown"
}

// gracefulDrain waits up to ShutdownBudget for in-flight handlers to
// finish. Cancels any that don't complete in time.
func (w *DispatchedWorker) gracefulDrain(parent context.Context) {
	deadline := time.Now().Add(w.cfg.ShutdownBudget)
	for w.inflightCnt.Load() > 0 && time.Now().Before(deadline) {
		select {
		case <-parent.Done():
			// Parent ctx was already canceled (that's why we got here),
			// but check periodically anyway.
		case <-time.After(100 * time.Millisecond):
		}
	}
	if w.inflightCnt.Load() > 0 {
		w.cfg.Logger.Warn("dispatched worker: shutdown budget exceeded, canceling handlers",
			"remaining", w.inflightCnt.Load())
		w.inflightMu.Lock()
		for _, cancel := range w.inflight {
			cancel()
		}
		w.inflightMu.Unlock()
	}
}

// Inflight returns the current in-flight count. Exposed for tests and
// readyz endpoints.
func (w *DispatchedWorker) Inflight() int {
	return int(w.inflightCnt.Load())
}

// jitter returns base ± 25% of its value. ±25% breaks reconnect
// storms across hundreds of workers without making any one worker's
// backoff wildly unpredictable.
func jitter(base time.Duration) time.Duration {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	bits := binary.BigEndian.Uint64(buf[:])
	// frac ∈ [-1, 1), scaled to ±0.25.
	frac := float64(int64(bits)) / math.MaxInt64 * 0.25
	return time.Duration(float64(base) * (1 + frac))
}

// sendOnStream marshals one envelope and SendMsg's it.
func sendOnStream(stream grpc.ClientStream, env *WorkerEnvelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return stream.SendMsg(&dispatchedJSONMsg{Raw: raw})
}

// recvOnStream RecvMsg's one envelope into a DispatchEnvelope.
func recvOnStream(stream grpc.ClientStream) (*DispatchEnvelope, error) {
	in := new(dispatchedJSONMsg)
	if err := stream.RecvMsg(in); err != nil {
		return nil, err
	}
	var env DispatchEnvelope
	if err := json.Unmarshal(in.Raw, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// dispatchedJSONMsg + dispatchedJSONCodec mirror the server-side
// envelope/codec. We declare our own copy here so the SDK doesn't
// depend on internal/server packages (which would break the open
// SDK boundary).
type dispatchedJSONMsg struct {
	Raw []byte
}

type dispatchedJSONCodec struct{}

func (dispatchedJSONCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*dispatchedJSONMsg)
	if !ok {
		return nil, fmt.Errorf("dispatchedJSONCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (dispatchedJSONCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*dispatchedJSONMsg)
	if !ok {
		return fmt.Errorf("dispatchedJSONCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

// Name MUST match the server-side dispatchCodecName in
// internal/server/jobsdispatcher/grpc.go. gRPC selects the server's
// codec by this name via the content-type header; collision with the
// admin service's "json" codec previously caused the dispatcher to
// fail unmarshal at SessionAccepted.
func (dispatchedJSONCodec) Name() string { return "atl-json-dispatch" }

// MaxJobNamesPerOpen mirrors the server-side cap so the SDK doesn't
// send a too-large envelope and get summarily rejected. Sync these
// with internal/server/jobsdispatcher/proto.go.
const MaxJobNamesPerOpen = 1024

// WorkerEnvelope / DispatchEnvelope / nested message types are
// duplicated here from the server-side proto.go because the SDK
// can't depend on internal/server packages. Keep in sync.
type WorkerEnvelope struct {
	Open       *OpenSession   `json:"open,omitempty"`
	Heartbeat  *Heartbeat     `json:"heartbeat,omitempty"`
	Checkpoint *CheckpointMsg `json:"checkpoint,omitempty"`
	Ack        *Ack           `json:"ack,omitempty"`
	Complete   *Complete      `json:"complete,omitempty"`
	Fail       *Fail          `json:"fail,omitempty"`
}

type DispatchEnvelope struct {
	SessionAccepted *SessionAccepted `json:"session_accepted,omitempty"`
	Dispatch        *Dispatch        `json:"dispatch,omitempty"`
	Revoke          *Revoke          `json:"revoke,omitempty"`
	Goodbye         *Goodbye         `json:"goodbye,omitempty"`
}

type OpenSession struct {
	Queue       string   `json:"queue"`
	JobNames    []string `json:"job_names"`
	MaxInFlight int      `json:"max_in_flight"`
	PodID       string   `json:"pod_id,omitempty"`
	Version     string   `json:"version,omitempty"`
}

type Heartbeat struct {
	JobIDs []int64 `json:"job_ids"`
}

// CheckpointMsg is the wire shape for one progress report. Named
// with the Msg suffix to avoid colliding with the package-level
// jobs.Checkpoint() function. Sent immediately on the handler's
// jobs.Checkpoint(ctx, pct, msg) call — distinct from the auto-
// heartbeat tick so operators see new progress without waiting.
type CheckpointMsg struct {
	JobID int64  `json:"job_id"`
	Pct   int    `json:"pct,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

type Ack struct {
	JobID int64 `json:"job_id"`
}

type Complete struct {
	JobID int64 `json:"job_id"`
}

type Fail struct {
	JobID int64  `json:"job_id"`
	Error string `json:"error"`
	Retry bool   `json:"retry"`
}

type SessionAccepted struct {
	SessionID   string `json:"session_id"`
	LeaseTTLMS  int    `json:"lease_ttl_ms"`
	HeartbeatMS int    `json:"heartbeat_ms"`
}

type Dispatch struct {
	JobID        int64  `json:"job_id"`
	JobName      string `json:"job_name"`
	Queue        string `json:"queue"`
	Args         []byte `json:"args,omitempty"`
	Attempts     int    `json:"attempts"`
	MaxRetries   int    `json:"max_retries"`
	TimeoutMS    int    `json:"timeout_ms"`
	ScheduledFor string `json:"scheduled_for,omitempty"`
	EnqueuedAt   string `json:"enqueued_at,omitempty"`
	TraceCtx     []byte `json:"trace_ctx,omitempty"`
}

type Revoke struct {
	JobID  int64  `json:"job_id"`
	Reason string `json:"reason"`
}

type Goodbye struct {
	Reason string `json:"reason"`
}
