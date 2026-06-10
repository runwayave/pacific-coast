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

// AuthChecker mirrors CertBindingChecker: one construction site,
// two interceptor flavors. The auth allowlist itself doesn't carry
// per-request state (no cache), but routing both flavors through one
// Checker keeps the config in one place and prevents the unary/stream
// chains from drifting on exempt-prefix or allowlist changes.
type AuthChecker struct {
	check func(ctx context.Context, fullMethod string) error
}

// NewAuthChecker constructs the shared checker.
func NewAuthChecker(cfg AuthConfig) *AuthChecker {
	return &AuthChecker{check: buildAuthCheck(cfg)}
}

// Unary returns the unary interceptor flavor.
func (c *AuthChecker) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := c.check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the streaming interceptor flavor. Applied at stream
// open; the caller identity is bound at TLS handshake and can't
// change mid-stream.
func (c *AuthChecker) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := c.check(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// NewAuth is a backwards-compat wrapper. Prefer NewAuthChecker.
func NewAuth(cfg AuthConfig) grpc.UnaryServerInterceptor {
	return NewAuthChecker(cfg).Unary()
}

// NewAuthStream is a backwards-compat wrapper. Prefer NewAuthChecker.
func NewAuthStream(cfg AuthConfig) grpc.StreamServerInterceptor {
	return NewAuthChecker(cfg).Stream()
}

// buildAuthCheck extracts the allowlist gate so both interceptor
// flavors share one implementation. Returns a stateless closure;
// safe to reuse across goroutines.
func buildAuthCheck(cfg AuthConfig) func(ctx context.Context, fullMethod string) error {
	enforce := cfg.Enforce
	callerFn := cfg.CallerFromContext
	if callerFn == nil {
		callerFn = func(context.Context) string { return "anonymous" }
	}
	// Copy the exempt list so a later mutation by the caller can't
	// silently expand the bypass surface.
	exempt := append([]string(nil), cfg.ExemptPrefixes...)
	return func(ctx context.Context, fullMethod string) error {
		if !enforce {
			return nil
		}
		for _, p := range exempt {
			if strings.HasPrefix(fullMethod, p) {
				return nil
			}
		}
		caller := callerFn(ctx)
		if caller == "" || caller == "anonymous" {
			return status.Error(codes.Unauthenticated, "no caller identity")
		}
		if cfg.Allowlist == nil || !cfg.Allowlist.Allows(caller) {
			return status.Errorf(codes.PermissionDenied, "caller %q not in allowlist", caller)
		}
		return nil
	}
}
