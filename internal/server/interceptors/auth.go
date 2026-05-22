package interceptors

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Allower is the slice of internal/auth.CallerAllowlist that the auth
// interceptor consumes. Kept narrow so tests can pass a fake without
// importing internal/auth.
type Allower interface {
	Allows(caller string) bool
}

// AuthConfig parameterises the auth interceptor.
type AuthConfig struct {
	// Allowlist is the set of registered callers. Required when Enforce
	// is true; ignored when Enforce is false.
	Allowlist Allower

	// Enforce gates the entire check. When false the interceptor is a
	// no-op — useful for local development without mTLS. Production
	// flips this on by setting it from cfg.TLSCertFile != "".
	Enforce bool

	// CallerFromContext extracts the resolved caller identity from ctx
	// (populated by an earlier interceptor — resolveCallerInterceptor
	// in cmd/server). The empty string and "anonymous" are treated as
	// "no identity" and rejected when Enforce is on.
	CallerFromContext func(context.Context) string

	// ExemptPrefixes is the set of FullMethod prefixes that bypass the
	// allowlist check. Typical entries: the admin RPCs (so new callers
	// can bootstrap by registering their schema), gRPC health (k8s
	// probes), and gRPC reflection (debugging tools).
	ExemptPrefixes []string
}

// NewAuth returns a unary interceptor that rejects RPCs from callers
// not present in the allowlist. When AuthConfig.Enforce is false the
// interceptor is a no-op and the chain is identical to one without
// auth wired in.
//
// Place this AFTER resolveCallerInterceptor in the chain so the caller
// identity is already populated on ctx by the time auth runs.
func NewAuth(cfg AuthConfig) grpc.UnaryServerInterceptor {
	enforce := cfg.Enforce
	callerFn := cfg.CallerFromContext
	if callerFn == nil {
		callerFn = func(context.Context) string { return "anonymous" }
	}
	// Copy the exempt list so a later mutation by the caller can't
	// silently expand the bypass surface.
	exempt := append([]string(nil), cfg.ExemptPrefixes...)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !enforce {
			return handler(ctx, req)
		}
		for _, p := range exempt {
			if strings.HasPrefix(info.FullMethod, p) {
				return handler(ctx, req)
			}
		}
		caller := callerFn(ctx)
		if caller == "" || caller == "anonymous" {
			return nil, status.Error(codes.Unauthenticated, "no caller identity")
		}
		if cfg.Allowlist == nil || !cfg.Allowlist.Allows(caller) {
			return nil, status.Errorf(codes.PermissionDenied, "caller %q not in allowlist", caller)
		}
		return handler(ctx, req)
	}
}
