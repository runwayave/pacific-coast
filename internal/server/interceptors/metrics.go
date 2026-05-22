package interceptors

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/obs"
)

// NewMetrics returns a unary interceptor that records request duration
// and count for every gRPC call. Method and grpc status code are the
// labels; both come from closed sets so cardinality is bounded.
//
// Recording is done via defer so a panicked handler still shows up in
// the histogram (with code=Unknown), even though the recoveryInterceptor
// that converts the panic to an error sits closer to the outside of the
// chain. Place this near the top of the chain (after recovery, before
// rate-limit) so the histogram captures the time spent in auth +
// rate-limit + handler — the latency a caller actually sees.
func NewMetrics() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		code := codes.Unknown.String()
		defer func() {
			obs.GRPCRequestDuration.WithLabelValues(info.FullMethod, code).Observe(time.Since(start).Seconds())
			obs.GRPCRequestsTotal.WithLabelValues(info.FullMethod, code).Inc()
		}()
		resp, err := handler(ctx, req)
		code = status.Code(err).String()
		return resp, err
	}
}
