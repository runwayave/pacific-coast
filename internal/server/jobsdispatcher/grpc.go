// gRPC wiring for the WorkerDispatch service.
//
// One bidi streaming RPC: WorkerSession. JSON envelopes via the
// `jsonMsg` codec atlantis already registers from
// internal/server/admin/grpc.go's init — no second codec needed.
//
// The HandlerType (`(*WorkerDispatchServer)(nil)`) is the conformance
// interface gRPC's RegisterService uses for reflection-style runtime
// checks. A compile-time `var _ WorkerDispatchServer = (*Dispatcher)(nil)`
// at the bottom enforces *Dispatcher satisfies it.

package jobsdispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// dispatchCodecName is the gRPC codec name negotiated by the
// WorkerDispatch stream. Distinct from the admin service's "json"
// codec so the two codecs can target different envelope types
// without colliding in the global encoding registry.
const dispatchCodecName = "atl-json-dispatch"

// dispatchJSONCodec marshals jobsdispatcher.jsonMsg envelopes. The
// admin package's "json" codec only knows *admin.jsonMsg; mounting
// the dispatcher stream against the admin codec produced the
// "cannot unmarshal into *jobsdispatcher.jsonMsg" error that
// surfaced as soon as a worker actually connected. Registering this
// codec at init under a distinct name lets gRPC's content-type
// negotiation route dispatched-worker traffic to it specifically.
type dispatchJSONCodec struct{}

func (dispatchJSONCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*jsonMsg)
	if !ok {
		return nil, fmt.Errorf("dispatchJSONCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (dispatchJSONCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*jsonMsg)
	if !ok {
		return fmt.Errorf("dispatchJSONCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (dispatchJSONCodec) Name() string { return dispatchCodecName }

func init() { encoding.RegisterCodecV2(dispatchJSONCodec{}) }

// WorkerDispatchServer is the typed interface gRPC.RegisterService
// uses for runtime conformance checking. Mirrors the AdminServer
// pattern in internal/server/admin/grpc.go. The handler signature
// matches grpc.ServiceDesc's Streams.Handler — a raw ServerStream
// that we adapt via RecvMsg / SendMsg with the jsonMsg envelope.
type WorkerDispatchServer interface {
	WorkerSession(stream grpc.ServerStream) error
}

// Compile-time check that *Dispatcher satisfies the interface. Adding
// or renaming WorkerSession breaks build here, not in production.
var _ WorkerDispatchServer = (*Dispatcher)(nil)

// Register binds the WorkerDispatch service to a gRPC server.
func Register(srv *grpc.Server, d *Dispatcher) {
	srv.RegisterService(&serviceDesc, d)
}

// serviceDesc declares the one streaming RPC. Methods is empty —
// everything is bidi-streaming.
var serviceDesc = grpc.ServiceDesc{
	ServiceName: "atlantis.workerdispatch.v1.WorkerDispatch",
	HandlerType: (*WorkerDispatchServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "WorkerSession",
			Handler:       handleWorkerSession,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "atlantis/workerdispatch/v1/workerdispatch.proto",
}

// jsonMsg is the wire envelope shared with the admin service. The
// jsonCodec registered at admin package init handles marshal/unmarshal.
type jsonMsg struct {
	Raw []byte
}

// handleWorkerSession is the gRPC stream entry. Receives the first
// envelope (must be Open), validates authz, registers the session,
// then runs the recv loop (for Heartbeat/Ack/Complete/Fail) and
// the sender loop (for Dispatch/Revoke/Goodbye) concurrently.
func handleWorkerSession(srv any, stream grpc.ServerStream) error {
	d, ok := srv.(*Dispatcher)
	if !ok {
		return status.Errorf(codes.Internal, "handler bound to wrong type %T", srv)
	}
	return d.WorkerSession(stream)
}

// WorkerSession is the streaming handler. Called via the ServerStream
// adapter in handleWorkerSession; runs until the worker disconnects
// or the dispatcher shuts down.
//
// stream is typed loosely as ServerStream because gRPC's
// stream-handler signature predates generics; we use SendMsg / RecvMsg
// with the jsonMsg envelope.
func (d *Dispatcher) WorkerSession(stream grpc.ServerStream) error {
	defer func() {
		if rec := recover(); rec != nil {
			d.cfg.Logger.Error("dispatcher: WorkerSession panic", "panic", rec)
		}
	}()

	// 1. First envelope must be Open.
	openEnv, err := recvEnvelope(stream)
	if err != nil {
		return err
	}
	if openEnv.Open == nil {
		return status.Error(codes.InvalidArgument, "first envelope must be Open")
	}
	open := openEnv.Open
	if open.Queue == "" {
		return status.Error(codes.InvalidArgument, "open.queue is required")
	}
	if len(open.JobNames) == 0 {
		return status.Error(codes.InvalidArgument, "open.job_names is required")
	}
	if len(open.JobNames) > MaxJobNamesPerOpen {
		return status.Errorf(codes.InvalidArgument,
			"open.job_names exceeds max %d", MaxJobNamesPerOpen)
	}

	caller := d.cfg.CallerFromContext(stream.Context())

	// 2. Resolve operator-configured aliases for this caller. Aliases
	// extend the visible_to match set without requiring schema edits
	// (PostgreSQL-roles / AD-SID pattern). A nil AliasLoader degrades
	// to no-alias matching — back-compat for deployments not using
	// the feature.
	var aliases []string
	if d.cfg.AliasLoader != nil {
		var aliasErr error
		aliases, aliasErr = d.cfg.AliasLoader(stream.Context(), caller)
		if aliasErr != nil {
			// Aliases are an authorization aid, not a hard requirement.
			// Log + continue with no aliases rather than fail-closing,
			// so a transient DB blip on the aliases table doesn't take
			// down every dispatched worker. If the worker's CN alone
			// is enough to satisfy visible_to, the session still opens.
			d.cfg.Logger.Warn("dispatcher: alias load at Open",
				"caller", caller, "err", aliasErr)
		}
	}

	// 3. Authz. Reject the whole session if any requested job is out of scope.
	ir, irErr := d.cfg.IRLoader(stream.Context())
	if irErr != nil {
		d.cfg.Logger.Warn("dispatcher: IR load at Open", "caller", caller, "err", irErr)
		return status.Error(codes.FailedPrecondition, "IR unavailable")
	}
	if err := CheckWorkerAuthz(caller, aliases, open.JobNames, ir); err != nil {
		d.cfg.Logger.Info("dispatcher: authz rejected at Open",
			"caller", caller, "aliases", aliases, "queue", open.Queue, "err", err)
		return err
	}

	// 4. Build the per-job heartbeat override map from the IR. Only
	// populated for jobs the worker declared they handle — saves a
	// lookup per dispatch and bounds the per-session memory footprint.
	var perJobHeartbeat map[string]time.Duration
	for i := range ir.Jobs {
		j := &ir.Jobs[i]
		if j.HeartbeatMS <= 0 {
			continue
		}
		id := j.ID()
		// Only retain overrides for jobs this session handles —
		// other entries can never apply per the visible_to gate.
		isHandled := false
		for _, declared := range open.JobNames {
			if declared == id {
				isHandled = true
				break
			}
		}
		if !isHandled {
			continue
		}
		if perJobHeartbeat == nil {
			perJobHeartbeat = make(map[string]time.Duration, 2)
		}
		perJobHeartbeat[id] = time.Duration(j.HeartbeatMS) * time.Millisecond
	}

	// 5. Register the session + start the batched lease processor.
	s := newSession(open, caller, aliases, perJobHeartbeat)
	d.register(s)
	defer d.unregister(stream.Context(), s, "stream_closed")

	// Lease processor runs for the lifetime of the stream. Cancels
	// on stream context cancel; final flush handled inside.
	go d.runLeaseProcessor(stream.Context(), s)

	// 4. Send SessionAccepted.
	leaseTTL := int(d.cfg.HeartbeatBudget.Milliseconds())
	if err := sendEnvelope(stream, &DispatchEnvelope{
		SessionAccepted: &SessionAccepted{
			SessionID:   s.id,
			LeaseTTLMS:  leaseTTL,
			HeartbeatMS: leaseTTL / 3,
		},
	}); err != nil {
		return err
	}
	d.cfg.Logger.Info("dispatcher: session opened",
		"session", s.id, "caller", caller, "queue", open.Queue,
		"job_names", len(open.JobNames), "max_in_flight", s.maxInFlight)
	s.appendEvent(sessionEvent{
		At: time.Now(), Kind: "session_opened", Note: caller,
	})

	// 5. Start the sender goroutine pulling from outbox.
	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- d.runSender(stream, s)
	}()

	// 6. Recv loop.
	violationCount := 0
	for {
		env, err := recvEnvelope(stream)
		if err != nil {
			// Stream-closing error. Drain the sender; defer unregister releases rows.
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
				<-sendErrCh
				return nil
			}
			<-sendErrCh
			return err
		}
		switch {
		case env.Heartbeat != nil:
			d.handleHeartbeat(stream.Context(), s, env.Heartbeat)
		case env.Checkpoint != nil:
			d.handleCheckpoint(stream.Context(), s, env.Checkpoint)
		case env.Ack != nil:
			d.handleAck(s, env.Ack)
		case env.Complete != nil:
			d.handleComplete(stream.Context(), s, env.Complete)
		case env.Fail != nil:
			d.handleFail(stream.Context(), s, env.Fail)
		case env.Open != nil:
			// Double-Open is malformed; one Open per stream.
			violationCount++
			if violationCount >= 3 {
				return status.Error(codes.InvalidArgument, "repeated protocol violations")
			}
			d.cfg.Logger.Warn("dispatcher: duplicate Open ignored", "session", s.id)
			s.appendEvent(sessionEvent{
				At: time.Now(), Kind: "protocol_violation", Note: "duplicate_open",
			})
		default:
			violationCount++
			if violationCount >= 3 {
				return status.Error(codes.InvalidArgument, "repeated protocol violations")
			}
			s.appendEvent(sessionEvent{
				At: time.Now(), Kind: "protocol_violation", Note: "empty_envelope",
			})
		}
	}
}

// runSender pulls envelopes off s.outbox and writes them to the wire.
// Returns when:
//   - outbox is closed (won't happen — outbox is unbuffered-by-default but
//     we use a buffered chan and never close it)
//   - the stream's context cancels
//   - the session is signaled closed
//
// Slow sends (worker not Recv-ing) are bounded: we use SetWriteDeadline-
// like semantics by racing send against the close signal.
func (d *Dispatcher) runSender(stream grpc.ServerStream, s *session) error {
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-s.closeCh:
			return nil
		case env, ok := <-s.outbox:
			if !ok {
				return nil
			}
			if err := sendEnvelope(stream, env); err != nil {
				// Worker is gone. Mark session closed so the recv loop
				// also exits.
				s.close()
				return err
			}
		}
	}
}

// sendEnvelope marshals a DispatchEnvelope and writes it as a jsonMsg.
func sendEnvelope(stream grpc.ServerStream, env *DispatchEnvelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	return stream.SendMsg(&jsonMsg{Raw: raw})
}

// recvEnvelope reads the next jsonMsg from the wire and unmarshals it
// into a WorkerEnvelope.
func recvEnvelope(stream grpc.ServerStream) (*WorkerEnvelope, error) {
	in := new(jsonMsg)
	if err := stream.RecvMsg(in); err != nil {
		return nil, err
	}
	var env WorkerEnvelope
	if err := json.Unmarshal(in.Raw, &env); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unmarshal envelope: %v", err)
	}
	return &env, nil
}

// The JSON envelope codec used on this stream is the one registered
// at internal/server/admin/grpc.go's init — single shared codec named
// "json" across atlantis's gRPC surface. The dispatcher's grpc.go
// reuses it via the jsonMsg type defined above.
