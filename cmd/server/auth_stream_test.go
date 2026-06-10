package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ctxWithCertCN builds a context carrying a fake mTLS peer whose
// leaf cert has the supplied CN. Matches the layout resolveCaller
// reads: peer.Peer → credentials.TLSInfo → tls.ConnectionState.
func ctxWithCertCN(cn string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	p := &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
	}}}
	return peer.NewContext(context.Background(), p)
}

// ---------------------------------------------------------------------------
// fakeServerStream — minimal grpc.ServerStream for testing the
// stream-flavor interceptors defined alongside their unary siblings
// in auth.go.
// ---------------------------------------------------------------------------

type fakeServerStream struct{ ctx context.Context }

func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(any) error            { return nil }
func (s *fakeServerStream) RecvMsg(any) error            { return nil }

// ---------------------------------------------------------------------------
// resolveCallerStreamInterceptor
// ---------------------------------------------------------------------------

// TestResolveCallerStream_PopulatesContextFromCertCN confirms the
// streaming flavor extracts the cert CN exactly like the unary
// version. Without this, every WorkerSession would land at the
// dispatcher's authz check as "anonymous" — which is the bug PR 2
// shipped with and this PR fixes.
func TestResolveCallerStream_PopulatesContextFromCertCN(t *testing.T) {
	streamCtx := ctxWithCertCN("vendor")

	var seenCaller string
	handler := func(_ any, ss grpc.ServerStream) error {
		seenCaller = callerFromContext(ss.Context())
		return nil
	}
	intr := resolveCallerStreamInterceptor()
	err := intr(nil, &fakeServerStream{ctx: streamCtx}, &grpc.StreamServerInfo{FullMethod: "/x/y"}, handler)
	if err != nil {
		t.Fatalf("interceptor returned: %v", err)
	}
	if seenCaller != "vendor" {
		t.Errorf("handler saw caller %q, want %q", seenCaller, "vendor")
	}
}

// TestResolveCallerStream_AnonymousFallback confirms the absence of
// a cert (insecure dev) falls back to "anonymous" exactly like the
// unary version.
func TestResolveCallerStream_AnonymousFallback(t *testing.T) {
	var seenCaller string
	handler := func(_ any, ss grpc.ServerStream) error {
		seenCaller = callerFromContext(ss.Context())
		return nil
	}
	intr := resolveCallerStreamInterceptor()
	err := intr(nil, &fakeServerStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/y"}, handler)
	if err != nil {
		t.Fatalf("interceptor returned: %v", err)
	}
	if seenCaller != "anonymous" {
		t.Errorf("no-cert handler saw %q, want anonymous", seenCaller)
	}
}

// ---------------------------------------------------------------------------
// recoveryStreamInterceptor
// ---------------------------------------------------------------------------

// TestRecoveryStream_PanicConvertedToInternal confirms a handler
// panic doesn't escape the interceptor. For long-lived streams
// (WorkerSession runs for hours), a panic taking down the whole
// server would be a single-malformed-envelope crash vector.
func TestRecoveryStream_PanicConvertedToInternal(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	intr := recoveryStreamInterceptor(log)
	panickingHandler := func(_ any, _ grpc.ServerStream) error {
		panic("boom")
	}
	err := intr(nil, &fakeServerStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/y"}, panickingHandler)
	if status.Code(err) != codes.Internal {
		t.Errorf("panic should become Internal, got code=%v err=%v", status.Code(err), err)
	}
}

// TestRecoveryStream_NoPanicPassesThrough confirms the normal path
// is a no-op — the interceptor only wraps the call in defer/recover,
// it doesn't alter successful returns or non-panic errors.
func TestRecoveryStream_NoPanicPassesThrough(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	intr := recoveryStreamInterceptor(log)
	want := status.Error(codes.Unavailable, "service down")
	handler := func(_ any, _ grpc.ServerStream) error { return want }
	err := intr(nil, &fakeServerStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/y"}, handler)
	if err != want {
		t.Errorf("normal error should pass through unchanged, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// loggingStreamInterceptor
// ---------------------------------------------------------------------------

// TestLoggingStream_LogsCloseWithMethodCallerCode confirms a stream
// closure emits a log record with the canonical method/caller/code
// shape so a SIEM grep on `method=` finds both unary and stream events.
func TestLoggingStream_LogsCloseWithMethodCallerCode(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	streamCtx := context.WithValue(ctxWithCertCN("vendor"), callerKey{}, "vendor")

	intr := loggingStreamInterceptor(log)
	handlerErr := status.Error(codes.PermissionDenied, "no dice")
	handler := func(_ any, _ grpc.ServerStream) error { return handlerErr }
	_ = intr(nil, &fakeServerStream{ctx: streamCtx}, &grpc.StreamServerInfo{FullMethod: "/atl.WD/Session"}, handler)

	logged := buf.String()
	for _, needle := range []string{
		"method=/atl.WD/Session",
		"caller=vendor",
		"code=PermissionDenied",
	} {
		if !strings.Contains(logged, needle) {
			t.Errorf("missing %q in log; got:\n%s", needle, logged)
		}
	}
}

// TestLoggingStream_OKAtDebug confirms successful streams log at
// Debug, not Info — high-volume long-lived streams shouldn't flood
// Info-level operator dashboards.
func TestLoggingStream_OKAtDebug(t *testing.T) {
	var infoBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&infoBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	intr := loggingStreamInterceptor(log)
	handler := func(_ any, _ grpc.ServerStream) error { return nil }
	_ = intr(nil, &fakeServerStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/y"}, handler)
	if strings.Contains(infoBuf.String(), "method=/x/y") {
		t.Errorf("successful stream should not log at Info; got: %s", infoBuf.String())
	}
}
