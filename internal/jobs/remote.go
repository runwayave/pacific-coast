package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
)

// RemoteHandler dispatches a job to an external service over gRPC.
// The remote service implements the JSON-envelope HandleJob method:
//
//	POST /atlantis.handler.v1.Handler/HandleJob
//	Request:  {"JobName": "vendor.ShopifyImport", "Args": {...}}
//	Response: {"Error": ""}  (empty = success)
//
// Any language that can serve a gRPC endpoint with a JSON codec can
// act as a handler: Python, TypeScript, Rust, Java. The contract is
// deliberately minimal so the barrier to implementing a handler in
// a new language is one function, not a code generator.
type RemoteHandler struct {
	addr string

	mu   sync.Mutex
	conn *grpc.ClientConn
}

type remoteHandleRequest struct {
	JobName string          `json:"JobName"`
	Args    json.RawMessage `json:"Args"`
}

type remoteHandleResponse struct {
	Error string `json:"Error"`
}

// Handle implements Handler by calling the remote endpoint.
func (r *RemoteHandler) Handle(ctx context.Context, argsJSON []byte) error {
	conn, err := r.getConn()
	if err != nil {
		return fmt.Errorf("remote handler dial %s: %w", r.addr, err)
	}

	reqBody, err := json.Marshal(remoteHandleRequest{
		JobName: "", // filled by the caller via registry key
		Args:    argsJSON,
	})
	if err != nil {
		return fmt.Errorf("remote handler marshal: %w", err)
	}

	in := remoteMsg{Raw: reqBody}
	var out remoteMsg
	if err := conn.Invoke(ctx, "/atlantis.handler.v1.Handler/HandleJob", &in, &out); err != nil {
		return fmt.Errorf("remote handler invoke: %w", err)
	}

	var resp remoteHandleResponse
	if err := json.Unmarshal(out.Raw, &resp); err != nil {
		return fmt.Errorf("remote handler unmarshal response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("remote handler: %s", resp.Error)
	}
	return nil
}

func (r *RemoteHandler) getConn() (*grpc.ClientConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		return r.conn, nil
	}
	conn, err := grpc.NewClient(r.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(remoteCodec{})),
	)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

// RegisterRemote binds a remote endpoint as the handler for a job.
// The worker treats it identically to a local handler: Lookup
// returns the RemoteHandler, handleOne calls Handle, the remote
// endpoint receives the args JSON and returns success or error.
func (reg *Registry) RegisterRemote(jobID, addr string) {
	reg.Register(jobID, &RemoteHandler{addr: addr})
}

// remoteMsg + remoteCodec mirror the JSON-envelope pattern used by
// the admin gRPC surface. The codec is registered per-connection
// (not globally) so it doesn't interfere with the admin service's
// own codec registration.
type remoteMsg struct{ Raw []byte }

type remoteCodec struct{}

func (remoteCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*remoteMsg)
	if !ok {
		return nil, fmt.Errorf("remoteCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (remoteCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*remoteMsg)
	if !ok {
		return fmt.Errorf("remoteCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (remoteCodec) Name() string { return "json" }
