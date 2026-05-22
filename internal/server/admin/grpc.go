package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
)

// jsonCodec mirrors the codec the `pc` CLI installs on the client side
// (cmd/pc/client.go). Registering here means a `pc` invocation can use
// `ForceCodecV2(jsonCodec{})` and the server will negotiate to JSON.
//
// The codec is registered once at package init.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*jsonMsg)
	if !ok {
		return nil, fmt.Errorf("jsonCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (jsonCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*jsonMsg)
	if !ok {
		return fmt.Errorf("jsonCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (jsonCodec) Name() string { return "json" }

func init() { encoding.RegisterCodecV2(jsonCodec{}) }

// gRPC wiring for the Admin service.
//
// Hand-rolled rather than buf-generated because the Admin service is
// internal infrastructure that bootstraps the entity codegen pipeline;
// wiring it to its own buf-generated client would either need a separate
// proto module or hit a chicken-and-egg with the entity emitter. The
// hand-rolled ServiceDesc uses JSON envelopes — the wire shape
// (POST /atlantis.admin.v1.Admin/RPC with a JSON body) is stable for the
// `pc` CLI.

// AdminServer is the typed interface gRPC's RegisterService machinery
// uses for runtime conformance checking. It mirrors what a buf-generated
// stub would emit — required because grpc.ServiceDesc.HandlerType must
// be a pointer to an interface (gRPC calls reflect.Type.Implements on it,
// which panics on a non-interface type). *Service satisfies this
// interface by carrying the same method set.
type AdminServer interface {
	PlanSchema(context.Context, PlanRequest) (*PlanResponse, error)
	ApplyMigration(context.Context, ApplyRequest) (*ApplyResponse, error)
	GetMergedSchema(context.Context, GetMergedSchemaRequest) (*GetMergedSchemaResponse, error)
	BeginBackfillPlan(context.Context, BeginBackfillPlanRequest) (*BeginBackfillPlanResponse, error)
	GetBackfillStatus(context.Context, GetBackfillStatusRequest) (*GetBackfillStatusResponse, error)
}

// Compile-time check: *Service is the implementation of
// AdminServer. Adding an RPC = method on Service + entry in AdminServer +
// entry in serviceDesc.Methods. The compiler enforces all three stay in
// sync — drop one and this line won't compile.
var _ AdminServer = (*Service)(nil)

// Register binds the Admin service to a gRPC server.
func Register(srv *grpc.Server, svc *Service) {
	srv.RegisterService(&serviceDesc, svc)
}

// serviceDesc declares the wire-visible methods. Codec is JSON; gRPC sees
// each request/response as a `[]byte` it does not interpret. The handler
// unmarshals into our typed request structs, runs the logic, marshals the
// reply back.
var serviceDesc = grpc.ServiceDesc{
	ServiceName: "atlantis.admin.v1.Admin",
	HandlerType: (*AdminServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "PlanSchema", Handler: handlePlanSchema},
		{MethodName: "ApplyMigration", Handler: handleApplyMigration},
		{MethodName: "GetMergedSchema", Handler: handleGetMergedSchema},
		{MethodName: "BeginBackfillPlan", Handler: handleBeginBackfillPlan},
		{MethodName: "GetBackfillStatus", Handler: handleGetBackfillStatus},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "atlantis/admin/v1/admin.proto",
}

// jsonMsg is the wire payload type. gRPC's installed jsonCodec calls
// Marshal / Unmarshal on this; the message itself carries raw JSON bytes.
type jsonMsg struct {
	Raw []byte
}

// handlePlanSchema is the gRPC entry. Decodes JSON → PlanRequest, calls the
// pure-Go Service.PlanSchema, encodes PlanResponse → JSON. Interceptors
// (mTLS, caller resolution, logging) run upstream of this handler.
func handlePlanSchema(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req PlanRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokePlan(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/PlanSchema"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokePlan(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

func invokePlan(svc *Service, ctx context.Context, req *PlanRequest) (any, error) {
	resp, err := svc.PlanSchema(ctx, *req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleApplyMigration(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req ApplyRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeApply(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/ApplyMigration"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeApply(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

func invokeApply(svc *Service, ctx context.Context, req *ApplyRequest) (any, error) {
	resp, err := svc.ApplyMigration(ctx, *req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// handleGetMergedSchema is the gRPC entry for the `tide pull` flow. Same
// JSON-envelope shape as the other admin RPCs.
func handleGetMergedSchema(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetMergedSchemaRequest
	// An empty body is a valid "give me everything" request — don't fail
	// when the client doesn't send the SinceVersion field.
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetMergedSchema(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetMergedSchema"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetMergedSchema(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

func invokeGetMergedSchema(svc *Service, ctx context.Context, req *GetMergedSchemaRequest) (any, error) {
	resp, err := svc.GetMergedSchema(ctx, *req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleBeginBackfillPlan(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req BeginBackfillPlanRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeBeginBackfillPlan(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/BeginBackfillPlan"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeBeginBackfillPlan(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

func invokeBeginBackfillPlan(svc *Service, ctx context.Context, req *BeginBackfillPlanRequest) (any, error) {
	resp, err := svc.BeginBackfillPlan(ctx, *req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetBackfillStatus(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetBackfillStatusRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetBackfillStatus(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetBackfillStatus"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetBackfillStatus(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

func invokeGetBackfillStatus(svc *Service, ctx context.Context, req *GetBackfillStatusRequest) (any, error) {
	resp, err := svc.GetBackfillStatus(ctx, *req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}
