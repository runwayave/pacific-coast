package interceptors

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubAllower map[string]struct{}

func (s stubAllower) Allows(caller string) bool {
	_, ok := s[caller]
	return ok
}

func okHandler(_ context.Context, _ any) (any, error) { return "ok", nil }

type testCallerKey struct{}

func callCtx(caller string) context.Context {
	return context.WithValue(context.Background(), testCallerKey{}, caller)
}

func callerFrom(ctx context.Context) string {
	v, _ := ctx.Value(testCallerKey{}).(string)
	if v == "" {
		return "anonymous"
	}
	return v
}

func runAuth(t *testing.T, cfg AuthConfig, method, caller string) (any, error) {
	t.Helper()
	cfg.CallerFromContext = callerFrom
	intr := NewAuth(cfg)
	info := &grpc.UnaryServerInfo{FullMethod: method}
	return intr(callCtx(caller), nil, info, okHandler)
}

func TestAuth_DisabledIsNoOp(t *testing.T) {
	resp, err := runAuth(t, AuthConfig{Enforce: false}, "/x.Y/Z", "anonymous")
	if err != nil || resp != "ok" {
		t.Errorf("disabled auth should pass through: resp=%v err=%v", resp, err)
	}
}

func TestAuth_ExemptPrefixBypasses(t *testing.T) {
	cfg := AuthConfig{
		Enforce:        true,
		Allowlist:      stubAllower{},
		ExemptPrefixes: []string{"/atlantis.admin.v1.Admin/"},
	}
	resp, err := runAuth(t, cfg, "/atlantis.admin.v1.Admin/PlanSchema", "newcomer")
	if err != nil || resp != "ok" {
		t.Errorf("admin RPC should bypass allowlist: resp=%v err=%v", resp, err)
	}
}

func TestAuth_AnonymousRejected(t *testing.T) {
	cfg := AuthConfig{
		Enforce:   true,
		Allowlist: stubAllower{"alice": {}},
	}
	_, err := runAuth(t, cfg, "/x.Y/Z", "anonymous")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("anonymous should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuth_RegisteredCallerPasses(t *testing.T) {
	cfg := AuthConfig{
		Enforce:   true,
		Allowlist: stubAllower{"alice": {}},
	}
	resp, err := runAuth(t, cfg, "/x.Y/Z", "alice")
	if err != nil || resp != "ok" {
		t.Errorf("registered caller should pass: resp=%v err=%v", resp, err)
	}
}

func TestAuth_UnregisteredCallerRejected(t *testing.T) {
	cfg := AuthConfig{
		Enforce:   true,
		Allowlist: stubAllower{"alice": {}},
	}
	_, err := runAuth(t, cfg, "/x.Y/Z", "mallory")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("unregistered caller should be PermissionDenied, got %v", status.Code(err))
	}
}

func TestAuth_NilAllowlistRejects(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: nil}
	_, err := runAuth(t, cfg, "/x.Y/Z", "alice")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("nil allowlist should reject, got %v", status.Code(err))
	}
}
