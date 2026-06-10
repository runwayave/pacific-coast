// Stream interceptor flavors of the security-critical chain. Mirrors
// the unary versions in this package so the streaming RPC introduced
// by jobsdispatcher (and any future streaming RPCs) gets the same
// auth + cert-binding walls as every admin unary RPC.
//
// Background: PR 2's WorkerDispatch was the first streaming RPC in
// atlantis. The original auth.go comment noted that the unary
// interceptors were "identical in shape but not needed because no
// RPC is streaming yet." This file backfills the streaming halves.
//
// Pattern: each interceptor extracts its core check into a shared
// helper inside the same source file as its unary partner; the stream
// wrapper here calls that helper and forwards to the handler via a
// ctxStream that carries the modified context. The handler always
// reads identity via stream.Context() so the wiring stays
// indistinguishable between unary and stream from the handler's
// point of view.

package interceptors

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/obs"
)

// ctxStream wraps a grpc.ServerStream so that the handler sees a
// modified Context() — used to propagate the resolved caller from
// the stream-resolveCaller interceptor into downstream handlers and
// auth checks that read ctx.Value(callerKey{}).
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the modified context. All other ServerStream
// methods (RecvMsg, SendMsg, etc.) pass through to the embedded
// stream unchanged.
func (s *ctxStream) Context() context.Context { return s.ctx }

// WithStreamContext returns a ServerStream that reports the supplied
// ctx from Context(). Exported so cmd/server/auth.go's
// resolveCallerStreamInterceptor (which lives outside this package)
// can build the wrapper.
func WithStreamContext(ss grpc.ServerStream, ctx context.Context) grpc.ServerStream {
	return &ctxStream{ServerStream: ss, ctx: ctx}
}

// NewMetricsStream mirrors NewMetrics for streaming RPCs. The same
// closed-cardinality labels and recording semantics apply. For
// long-lived streams (e.g. WorkerSession) the histogram captures the
// total stream duration when it closes — useful for detecting
// pathologically short reconnect loops.
func NewMetricsStream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		code := codes.Unknown.String()
		defer func() {
			obs.GRPCRequestDuration.WithLabelValues(info.FullMethod, code).Observe(time.Since(start).Seconds())
			obs.GRPCRequestsTotal.WithLabelValues(info.FullMethod, code).Inc()
		}()
		err := handler(srv, ss)
		code = status.Code(err).String()
		return err
	}
}
