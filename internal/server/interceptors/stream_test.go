package interceptors

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Stream test scaffolding
// ---------------------------------------------------------------------------

// fakeServerStream implements grpc.ServerStream just enough for the
// interceptor tests. Context() returns the injected ctx; the rest are
// safe no-ops because the interceptors under test don't call them.
type fakeServerStream struct{ ctx context.Context }

func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(any) error            { return nil }
func (s *fakeServerStream) RecvMsg(any) error            { return nil }

// noopStreamHandler returns nil — the interceptors under test either
// reject before reaching the handler or pass through to it; either
// way the handler's body is uninteresting.
func noopStreamHandler(_ any, _ grpc.ServerStream) error { return nil }

func runAuthStream(t *testing.T, cfg AuthConfig, method, caller string) error {
	t.Helper()
	cfg.CallerFromContext = callerFrom
	intr := NewAuthChecker(cfg).Stream()
	info := &grpc.StreamServerInfo{FullMethod: method}
	ss := &fakeServerStream{ctx: callCtx(caller)}
	return intr(nil, ss, info, noopStreamHandler)
}

// ---------------------------------------------------------------------------
// AuthChecker.Stream tests — mirror the unary tests so the same
// allowlist semantics are confirmed on both surfaces.
// ---------------------------------------------------------------------------

func TestAuthStream_DisabledIsNoOp(t *testing.T) {
	if err := runAuthStream(t, AuthConfig{Enforce: false}, "/x.Y/Z", "anonymous"); err != nil {
		t.Errorf("disabled stream auth should pass: %v", err)
	}
}

func TestAuthStream_ExemptPrefixBypasses(t *testing.T) {
	cfg := AuthConfig{
		Enforce:        true,
		Allowlist:      stubAllower{},
		ExemptPrefixes: []string{"/atlantis.admin.v1.Admin/"},
	}
	if err := runAuthStream(t, cfg, "/atlantis.admin.v1.Admin/PlanSchema", "newcomer"); err != nil {
		t.Errorf("admin prefix exempt on stream: %v", err)
	}
}

func TestAuthStream_AnonymousRejected(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	err := runAuthStream(t, cfg, "/x.Y/Z", "anonymous")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("anonymous on stream should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthStream_RegisteredCallerPasses(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	if err := runAuthStream(t, cfg, "/x.Y/Z", "alice"); err != nil {
		t.Errorf("registered caller stream rejected: %v", err)
	}
}

func TestAuthStream_UnregisteredCallerRejected(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	err := runAuthStream(t, cfg, "/x.Y/Z", "mallory")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("unregistered stream caller should be PermissionDenied, got %v", status.Code(err))
	}
}

// ---------------------------------------------------------------------------
// CertBindingChecker tests — covers both Unary and Stream flavors plus
// the shared-cache property.
// ---------------------------------------------------------------------------

// peerCtx builds a context with a fake mTLS peer presenting the
// supplied raw cert bytes. Matches the layout leafCertFromContext
// reads (peer.Peer → credentials.TLSInfo → tls.ConnectionState).
func peerCtx(parent context.Context, rawCertBytes []byte) context.Context {
	cert := &x509.Certificate{Raw: rawCertBytes}
	p := &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}},
		},
	}
	return peer.NewContext(parent, p)
}

type cbLookupResult struct {
	exists      bool
	fingerprint []byte
	err         error
}

type recordingLookup struct {
	results map[string]cbLookupResult
	calls   atomic.Int64
}

func (r *recordingLookup) lookup(_ context.Context, caller string) (bool, []byte, error) {
	r.calls.Add(1)
	got, ok := r.results[caller]
	if !ok {
		return false, nil, nil
	}
	return got.exists, got.fingerprint, got.err
}

func runCertBindingUnary(t *testing.T, c *CertBindingChecker, ctx context.Context, method string) error {
	t.Helper()
	info := &grpc.UnaryServerInfo{FullMethod: method}
	_, err := c.Unary()(ctx, nil, info, okHandler)
	return err
}

func runCertBindingStream(t *testing.T, c *CertBindingChecker, ctx context.Context, method string) error {
	t.Helper()
	info := &grpc.StreamServerInfo{FullMethod: method}
	ss := &fakeServerStream{ctx: ctx}
	return c.Stream()(nil, ss, info, noopStreamHandler)
}

func TestCertBinding_DisabledIsNoOp(t *testing.T) {
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           false,
		CallerFromContext: callerFrom,
	})
	if err := runCertBindingStream(t, c, callCtx("alice"), "/x.Y/Z"); err != nil {
		t.Errorf("disabled stream cert binding should pass: %v", err)
	}
	if err := runCertBindingUnary(t, c, callCtx("alice"), "/x.Y/Z"); err != nil {
		t.Errorf("disabled unary cert binding should pass: %v", err)
	}
}

func TestCertBinding_AnonymousSkipped(t *testing.T) {
	// Anonymous reaches this interceptor in insecure dev mode. Auth
	// will reject downstream; this interceptor has nothing to do.
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            (&recordingLookup{}).lookup,
		CallerFromContext: callerFrom,
	})
	if err := runCertBindingStream(t, c, callCtx("anonymous"), "/x.Y/Z"); err != nil {
		t.Errorf("anonymous stream should skip cert binding, got %v", err)
	}
}

func TestCertBinding_ExemptCallerSkipped(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
		ExemptCallers:     []string{"atlantis-console"},
	})
	if err := runCertBindingStream(t, c, callCtx("atlantis-console"), "/x.Y/Z"); err != nil {
		t.Errorf("exempt caller on stream should pass without lookup: %v", err)
	}
	if rl.calls.Load() != 0 {
		t.Errorf("exempt caller should not trigger lookup, got %d calls", rl.calls.Load())
	}
}

func TestCertBinding_NoPeerCertRejected(t *testing.T) {
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            (&recordingLookup{}).lookup,
		CallerFromContext: callerFrom,
	})
	// Caller is set but no peer cert in context → listener mis-configured.
	err := runCertBindingStream(t, c, callCtx("alice"), "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("missing peer cert should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestCertBinding_UnknownCallerRejected(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), []byte("any-cert"))
	err := runCertBindingStream(t, c, ctx, "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("unknown caller should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestCertBinding_BootstrapWindowPasses(t *testing.T) {
	// Row exists but no fingerprint yet → operator registered the
	// caller but hasn't issued a cert. Accept any CA-signed cert
	// until the first binding is recorded.
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true, fingerprint: nil},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), []byte("any-cert"))
	if err := runCertBindingStream(t, c, ctx, "/x.Y/Z"); err != nil {
		t.Errorf("bootstrap window should pass: %v", err)
	}
}

func TestCertBinding_FingerprintMismatchRejected(t *testing.T) {
	storedFP := sha256.Sum256([]byte("real-cert-bytes"))
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true, fingerprint: storedFP[:]},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	// Present a DIFFERENT cert — fingerprint won't match the stored one.
	ctx := peerCtx(callCtx("alice"), []byte("rotated-cert-bytes"))
	err := runCertBindingStream(t, c, ctx, "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("fingerprint mismatch should be Unauthenticated, got %v err=%v", status.Code(err), err)
	}
}

func TestCertBinding_FingerprintMatchPasses(t *testing.T) {
	rawCert := []byte("real-cert-bytes")
	storedFP := sha256.Sum256(rawCert)
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true, fingerprint: storedFP[:]},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), rawCert)
	if err := runCertBindingStream(t, c, ctx, "/x.Y/Z"); err != nil {
		t.Errorf("fingerprint match should pass: %v", err)
	}
}

func TestCertBinding_LookupErrorRejected(t *testing.T) {
	// Transient lookup failures fail closed — caller can't auth via
	// a broken DB connection.
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {err: errors.New("db down")},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), []byte("any-cert"))
	err := runCertBindingStream(t, c, ctx, "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("lookup error should fail closed Unauthenticated, got %v", status.Code(err))
	}
}

// TestCertBinding_SharedCache pins the key promise of the Checker
// refactor: one Checker → shared cache across Unary() and Stream().
// Two interceptor calls for the same CN must hit the DB exactly
// once, regardless of which flavor opened the first call.
func TestCertBinding_SharedCacheAcrossUnaryAndStream(t *testing.T) {
	rawCert := []byte("shared-cert")
	storedFP := sha256.Sum256(rawCert)
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true, fingerprint: storedFP[:]},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
		CacheTTL:          5 * time.Second,
	})
	ctx := peerCtx(callCtx("alice"), rawCert)

	if err := runCertBindingUnary(t, c, ctx, "/unary.Path"); err != nil {
		t.Fatalf("unary call: %v", err)
	}
	if err := runCertBindingStream(t, c, ctx, "/stream.Path"); err != nil {
		t.Fatalf("stream call: %v", err)
	}
	if err := runCertBindingUnary(t, c, ctx, "/unary.Path"); err != nil {
		t.Fatalf("second unary call: %v", err)
	}
	if got := rl.calls.Load(); got != 1 {
		t.Errorf("shared cache should cause exactly 1 DB lookup; got %d", got)
	}
}

// TestCertBinding_SeparateCachesWithLegacyAPI confirms that the
// backwards-compat NewCertBinding + NewCertBindingStream pattern
// (used to be the only API) does NOT share a cache. Documents the
// behavior so a future reader knows the Checker pattern is the
// path to deduplication.
func TestCertBinding_SeparateCachesWithLegacyAPI(t *testing.T) {
	rawCert := []byte("legacy-cert")
	storedFP := sha256.Sum256(rawCert)
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true, fingerprint: storedFP[:]},
	}}
	cfg := CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	}

	unary := NewCertBinding(cfg)        // legacy wrapper → its own cache
	stream := NewCertBindingStream(cfg) // legacy wrapper → another cache

	ctx := peerCtx(callCtx("alice"), rawCert)
	unaryInfo := &grpc.UnaryServerInfo{FullMethod: "/u.X/Y"}
	streamInfo := &grpc.StreamServerInfo{FullMethod: "/s.X/Y"}

	if _, err := unary(ctx, nil, unaryInfo, okHandler); err != nil {
		t.Fatalf("legacy unary: %v", err)
	}
	if err := stream(nil, &fakeServerStream{ctx: ctx}, streamInfo, noopStreamHandler); err != nil {
		t.Fatalf("legacy stream: %v", err)
	}

	// Two separate caches → two separate DB hits. Documenting, not
	// asserting "should be 2" rigidly — what matters is that the
	// shared-cache test above passes with the Checker API.
	if got := rl.calls.Load(); got < 2 {
		t.Errorf("legacy API should NOT dedupe; expected >=2 lookups, got %d", got)
	}
}

// TestCertBinding_CertBindingChecker_StreamMatchesUnary confirms
// the two flavors agree on the verdict for every input. Important
// because the security contract relies on this equivalence — drift
// between flavors would let an attacker fail a unary check then
// retry on a stream.
func TestCertBinding_StreamMatchesUnary(t *testing.T) {
	rawCert := []byte("alice-cert")
	storedFP := sha256.Sum256(rawCert)
	cfg := CertBindingConfig{
		Enforce: true,
		Lookup: (&recordingLookup{results: map[string]cbLookupResult{
			"alice":   {exists: true, fingerprint: storedFP[:]},
			"mallory": {exists: false},
		}}).lookup,
		CallerFromContext: callerFrom,
	}

	cases := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{"valid", peerCtx(callCtx("alice"), rawCert), codes.OK},
		{"wrong_fingerprint", peerCtx(callCtx("alice"), []byte("rotated")), codes.Unauthenticated},
		{"unknown_caller", peerCtx(callCtx("mallory"), rawCert), codes.Unauthenticated},
		{"no_peer_cert", callCtx("alice"), codes.Unauthenticated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each iteration gets a fresh checker so the cache doesn't
			// hide differences between unary and stream code paths.
			c1 := NewCertBindingChecker(cfg)
			c2 := NewCertBindingChecker(cfg)
			unaryErr := runCertBindingUnary(t, c1, tc.ctx, "/u.X/Y")
			streamErr := runCertBindingStream(t, c2, tc.ctx, "/s.X/Y")
			if status.Code(unaryErr) != tc.wantCode {
				t.Errorf("unary code = %v, want %v", status.Code(unaryErr), tc.wantCode)
			}
			if status.Code(streamErr) != tc.wantCode {
				t.Errorf("stream code = %v, want %v", status.Code(streamErr), tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WithStreamContext + ctxStream wrapper test
// ---------------------------------------------------------------------------

// TestWithStreamContext_PropagatesValue pins that the ctxStream
// wrapper actually returns the supplied context — the whole point
// of the stream-resolveCaller interceptor relies on this.
func TestWithStreamContext_PropagatesValue(t *testing.T) {
	type k struct{}
	base := &fakeServerStream{ctx: context.Background()}
	wrapped := WithStreamContext(base, context.WithValue(context.Background(), k{}, "v"))
	got, _ := wrapped.Context().Value(k{}).(string)
	if got != "v" {
		t.Errorf("WithStreamContext: got value %q, want %q", got, "v")
	}
}

// ---------------------------------------------------------------------------
// AuthChecker StreamMatchesUnary
// ---------------------------------------------------------------------------

// Equivalent to the cert binding cross-flavor test: both flavors of
// the auth checker must reach identical verdicts for every input.
func TestAuth_StreamMatchesUnary(t *testing.T) {
	cfg := AuthConfig{
		Enforce:           true,
		Allowlist:         stubAllower{"alice": {}},
		ExemptPrefixes:    []string{"/atlantis.admin.v1.Admin/"},
		CallerFromContext: callerFrom,
	}

	cases := []struct {
		name     string
		caller   string
		method   string
		wantCode codes.Code
	}{
		{"in_allowlist", "alice", "/x.Y/Z", codes.OK},
		{"not_in_allowlist", "mallory", "/x.Y/Z", codes.PermissionDenied},
		{"anonymous", "anonymous", "/x.Y/Z", codes.Unauthenticated},
		{"exempt_prefix_for_unregistered", "mallory", "/atlantis.admin.v1.Admin/Foo", codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewAuthChecker(cfg)
			unaryInfo := &grpc.UnaryServerInfo{FullMethod: tc.method}
			streamInfo := &grpc.StreamServerInfo{FullMethod: tc.method}
			_, unaryErr := c.Unary()(callCtx(tc.caller), nil, unaryInfo, okHandler)
			streamErr := c.Stream()(nil, &fakeServerStream{ctx: callCtx(tc.caller)}, streamInfo, noopStreamHandler)
			if status.Code(unaryErr) != tc.wantCode {
				t.Errorf("unary code = %v, want %v", status.Code(unaryErr), tc.wantCode)
			}
			if status.Code(streamErr) != tc.wantCode {
				t.Errorf("stream code = %v, want %v", status.Code(streamErr), tc.wantCode)
			}
		})
	}
}
