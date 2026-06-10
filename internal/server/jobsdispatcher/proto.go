// Wire shapes for the WorkerDispatch bidi streaming RPC.
//
// Worker → server envelopes (WorkerEnvelope) and server → worker
// envelopes (DispatchEnvelope) are tagged unions: exactly one of the
// nested pointer fields is non-nil per envelope. JSON-codec friendly
// — the admin gRPC server's JSON envelope (internal/server/admin/grpc.go)
// uses the same shape, and we register against the same `jsonMsg`
// codec so atlantis's existing wire conventions extend cleanly.
//
// Why a tagged union instead of separate RPCs per message type? A
// single bidi stream is the cleanest way to multiplex (a) server-
// pushed Dispatch envelopes (b) worker-pushed Ack/Heartbeat/Complete/
// Fail envelopes over one TLS session per worker pod. Streaming RPCs
// give us free stream-close detection (worker death) and natural
// back-pressure (the server's Send blocks when the worker's flow-
// control window fills).
//
// JSON over gRPC is intentional: the admin service already uses this
// codec and `tide` / the console call it the same way. Adding a
// streaming JSON service is one more entry in the JSON envelope
// universe rather than introducing protobuf descriptors that the
// admin surface deliberately avoids.

package jobsdispatcher

// WorkerEnvelope is the client → server message shape. Exactly one of
// the variant pointer fields must be non-nil; envelopes with zero or
// multiple variants set are treated as malformed (see the protocol-
// violation handling in session.go).
type WorkerEnvelope struct {
	Open       *OpenSession `json:"open,omitempty"`
	Heartbeat  *Heartbeat   `json:"heartbeat,omitempty"`
	Checkpoint *Checkpoint  `json:"checkpoint,omitempty"`
	Ack        *Ack         `json:"ack,omitempty"`
	Complete   *Complete    `json:"complete,omitempty"`
	Fail       *Fail        `json:"fail,omitempty"`
}

// DispatchEnvelope is the server → client message shape. Same tagged-
// union shape as WorkerEnvelope.
type DispatchEnvelope struct {
	SessionAccepted *SessionAccepted `json:"session_accepted,omitempty"`
	Dispatch        *Dispatch        `json:"dispatch,omitempty"`
	Revoke          *Revoke          `json:"revoke,omitempty"`
	Goodbye         *Goodbye         `json:"goodbye,omitempty"`
}

// OpenSession is the worker's first envelope. Declares the queue this
// worker drains and the job names it can handle. The server validates
// authz against the worker's cert CN and the IR's `visible_to` field
// on each requested job; mismatch closes the stream with PermissionDenied.
//
// MaxInFlight is the worker's self-declared concurrency cap. Clamped
// server-side to [1, 256] so a typo can't cause unbounded dispatch.
//
// PodID is informational — shows up in logs and in the `claimed_by`
// column for forensics. Version is the SDK release the worker was
// built against, also informational.
type OpenSession struct {
	Queue       string   `json:"queue"`
	JobNames    []string `json:"job_names"`
	MaxInFlight int      `json:"max_in_flight"`
	PodID       string   `json:"pod_id,omitempty"`
	Version     string   `json:"version,omitempty"`
}

// Heartbeat is a batched lease bump. The worker reports every job it
// still considers in-flight; the dispatcher extends `claimed_until`
// for each. Length-capped server-side to defend against a malicious
// or buggy worker sending billions of ids.
type Heartbeat struct {
	JobIDs []int64 `json:"job_ids"`
}

// Checkpoint reports handler progress for one in-flight job. Distinct
// from Heartbeat so the persistence isn't gated on the auto-tick: when
// a handler finishes a stage and immediately wants the operator to
// see `progress_pct = 100`, the Checkpoint envelope flushes through
// the priority send channel without waiting for the next ~10s beat.
//
// Semantics on receipt:
//   - Lease is extended (same code path as Heartbeat — both feed the
//     batched lease processor).
//   - atlantis.jobs.progress_pct / progress_msg / progress_at are
//     updated for this row. Pct is clamped to [0, 100]; Msg is
//     truncated to MaxCheckpointMsgChars characters.
//   - A session ring-buffer event is appended so operators can see
//     the progress trail in the console session-detail view.
//
// Unlike Heartbeat, a Checkpoint for a single job_id is intentional:
// progress is per-row state. The SDK sends one Checkpoint per
// jobs.Checkpoint(ctx, pct, msg) call — handlers decide cadence.
type Checkpoint struct {
	JobID int64  `json:"job_id"`
	Pct   int    `json:"pct,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// Ack signals that the worker has started running the dispatched job.
// Distinguishes "claimed in PG" from "actually executing on the
// worker" — without it, a worker that crashes between Recv and
// handler-start would hold a lease until expiry. With Ack, the server
// can treat missing-Ack-within-LeaseTTL/2 as a dead worker and revoke.
type Ack struct {
	JobID int64 `json:"job_id"`
}

// Complete is the terminal success envelope. The server then writes
// status='complete' via the shared MarkComplete helper.
type Complete struct {
	JobID int64 `json:"job_id"`
}

// Fail is the terminal failure envelope. If Retry is true (the
// normal case for handler errors), the server's ReportFailure helper
// decides DLQ-vs-pending based on attempts/max_retries. If Retry is
// false, the row goes straight to atlantis.jobs_dead regardless of
// remaining attempts — operator-intent escape hatch for unrecoverable
// errors detected at the handler level (e.g., corrupted args).
type Fail struct {
	JobID int64  `json:"job_id"`
	Error string `json:"error"`
	Retry bool   `json:"retry"`
}

// SessionAccepted is the server's first reply, after authz passes.
// Carries the server-assigned session id (used in logs + claimed_by
// formatting) and the lease parameters the worker should target.
type SessionAccepted struct {
	SessionID   string `json:"session_id"`
	LeaseTTLMS  int    `json:"lease_ttl_ms"`
	HeartbeatMS int    `json:"heartbeat_ms"`
}

// Dispatch hands a claimed job to the worker. Args is the JSON-encoded
// args struct the typed Handler interface deserializes. TraceCtx
// carries the OTel parent for distributed tracing — the SDK resumes
// the trace before invoking the handler.
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

// Revoke tells the worker that the server has pulled the row back:
// the row's lease has been released and the worker SHOULD cancel its
// handler. Reasons are operator-facing strings, not enum values, so
// adding a new reason later doesn't require a wire-shape change.
type Revoke struct {
	JobID  int64  `json:"job_id"`
	Reason string `json:"reason"`
}

// Goodbye is the server's signal that it's closing the stream. After
// sending it, the server stops pushing Dispatch envelopes; the worker
// should finish current in-flight and exit. Used in graceful shutdown
// and operator-initiated Drain.
type Goodbye struct {
	Reason string `json:"reason"`
}

// Defensive limits applied at Open / Heartbeat / Checkpoint receive.
// A malicious or buggy worker can't blow up server memory by sending
// a million job names or a multi-megabyte progress message — these
// caps reject obviously-wrong envelopes before they consume any
// per-id machinery.
const (
	MaxJobNamesPerOpen      = 1024
	MaxHeartbeatIDsPerFrame = 4096
	MaxCheckpointMsgChars   = 256
	MinMaxInFlight          = 1
	MaxMaxInFlight          = 256
)
