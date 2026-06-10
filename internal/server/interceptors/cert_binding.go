package interceptors

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// CertBindingLookup returns the binding state for a caller. exists
// reports whether a caller_identities row is present; fingerprint is
// the 32-byte SHA-256 of the row's currently-active cert (nil when
// the row exists but no cert has been recorded yet — the back-compat
// branch for callers minted before binding existed).
//
// Returning a non-nil error fails open in the same way the lookup
// would if the row truly didn't exist — the interceptor logs the
// error and treats it as "unknown caller." Lookups MUST NOT block
// indefinitely; the caller is on the request hot path.
type CertBindingLookup func(ctx context.Context, caller string) (exists bool, fingerprint []byte, err error)

// CertBindingConfig parameterises the cert-binding interceptor.
type CertBindingConfig struct {
	// Lookup is the DB-backed (or test-faked) resolver for a caller's
	// stored fingerprint. Required when Enforce is true.
	Lookup CertBindingLookup

	// Enforce gates the entire check. Set to true exactly when mTLS is
	// configured — without TLS there's no peer cert to bind against.
	Enforce bool

	// CallerFromContext extracts the resolved caller identity from ctx
	// (populated by resolveCallerInterceptor).
	CallerFromContext func(context.Context) string

	// ExemptCallers is the set of CNs that always pass without a
	// fingerprint check. Reserved for the management plane (the console
	// CN, the signer CN) whose authentication is enforced at a higher
	// layer (session cookies + sudo + role).
	ExemptCallers []string

	// CacheTTL is how long a lookup result is held in process before
	// being re-read. Default 5s. Set to 0 to disable caching (tests).
	CacheTTL time.Duration

	// Log receives structured records of every reject decision plus
	// any transient lookup errors.
	Log *slog.Logger
}

// CertBindingChecker owns the cert-binding state shared across both
// the unary and stream interceptors: the TTL cache of CN -> stored
// fingerprint, the exempt-CN set, and the resolver callbacks. Mount
// via .Unary() and .Stream() on their respective chains; both
// methods consult the same cache so a stream RPC and a unary RPC for
// the same CN share lookup state — a single DB hit per CN per TTL
// window across the whole gRPC surface, not one per interceptor
// flavor.
//
// Lifecycle: construct once at server boot; safe to call .Unary()
// and .Stream() multiple times (each returns a fresh closure, but
// all closures from the same Checker share the underlying cache and
// config).
type CertBindingChecker struct {
	check func(ctx context.Context, fullMethod string) error
}

// NewCertBindingChecker constructs the shared checker. The supplied
// CertBindingConfig is captured by value; subsequent mutations on
// the original config don't affect the checker (defense against the
// "caller secretly expanded the exempt list" footgun).
func NewCertBindingChecker(cfg CertBindingConfig) *CertBindingChecker {
	return &CertBindingChecker{check: buildCertBindingCheck(cfg)}
}

// Unary returns the unary interceptor flavor.
func (c *CertBindingChecker) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := c.check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the streaming interceptor flavor. Applied at stream
// open; the peer cert is fixed at TLS handshake time, so re-checking
// on every envelope would be wasted work.
func (c *CertBindingChecker) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := c.check(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// NewCertBinding is a thin backwards-compat wrapper. New code should
// prefer NewCertBindingChecker so unary + stream callers share one
// cache. Calling NewCertBinding + NewCertBindingStream with the same
// config creates two independent caches; calling NewCertBindingChecker
// once and using .Unary() + .Stream() shares them.
func NewCertBinding(cfg CertBindingConfig) grpc.UnaryServerInterceptor {
	return NewCertBindingChecker(cfg).Unary()
}

// NewCertBindingStream is the backwards-compat sibling of NewCertBinding.
// See the note on NewCertBinding about cache sharing.
func NewCertBindingStream(cfg CertBindingConfig) grpc.StreamServerInterceptor {
	return NewCertBindingChecker(cfg).Stream()
}

// buildCertBindingCheck extracts the fingerprint comparison so both
// flavors share one implementation. Returns a closure that captures
// the cache + lookup so a single TTL bucket serves the whole gRPC
// surface.
func buildCertBindingCheck(cfg CertBindingConfig) func(ctx context.Context, fullMethod string) error {
	enforce := cfg.Enforce
	callerFn := cfg.CallerFromContext
	if callerFn == nil {
		callerFn = func(context.Context) string { return "anonymous" }
	}
	exempt := make(map[string]struct{}, len(cfg.ExemptCallers))
	for _, c := range cfg.ExemptCallers {
		exempt[c] = struct{}{}
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	cache := &bindingCache{ttl: ttl}

	return func(ctx context.Context, fullMethod string) error {
		if !enforce {
			return nil
		}
		caller := callerFn(ctx)
		// Anonymous reaches us in insecure dev mode (no mTLS configured
		// for the listener). The auth interceptor will reject it; this
		// interceptor has nothing meaningful to check.
		if caller == "" || caller == "anonymous" {
			return nil
		}
		if _, ok := exempt[caller]; ok {
			return nil
		}

		// Pull the presented leaf cert. With Enforce=true the listener
		// is tls.RequireAndVerifyClientCert, so a missing peer cert
		// here means the listener is mis-configured — fail closed.
		peerCert, ok := leafCertFromContext(ctx)
		if !ok {
			log.Warn("cert binding: no peer cert on enforced path", "caller", caller, "method", fullMethod)
			return status.Error(codes.Unauthenticated, "no peer certificate")
		}
		presented := sha256.Sum256(peerCert.Raw)

		exists, stored, err := cache.lookup(ctx, caller, cfg.Lookup)
		if err != nil {
			log.Error("cert binding: lookup", "caller", caller, "method", fullMethod, "err", err)
			return status.Error(codes.Unauthenticated, "caller binding unavailable")
		}
		if !exists {
			// Caller has no row — either never registered, or revoked.
			// Either way it can't authenticate. Distinguishing the two
			// would leak existence; one error code covers both.
			log.Info("cert binding: unknown caller", "caller", caller, "method", fullMethod)
			return status.Errorf(codes.Unauthenticated, "caller %q is not registered", caller)
		}
		if stored == nil {
			// Bootstrap window: row exists but no fingerprint recorded
			// yet (operator registered the caller, hasn't issued a cert
			// through the console). Accept any CA-signed cert until
			// the first console issuance binds the fingerprint.
			return nil
		}
		// subtle.ConstantTimeCompare so a timing oracle can't probe
		// fingerprint bytes one column at a time.
		if subtle.ConstantTimeCompare(stored, presented[:]) != 1 {
			log.Info("cert binding: fingerprint mismatch (cert superseded)",
				"caller", caller,
				"method", fullMethod,
			)
			return status.Errorf(codes.Unauthenticated, "cert superseded for caller %q", caller)
		}
		return nil
	}
}

// leafCertFromContext extracts the peer's leaf x509 cert from a
// resolved TLS connection. Returns false in non-TLS modes (where no
// cert was negotiated) so callers can choose how to react.
func leafCertFromContext(ctx context.Context) (cert leafCert, ok bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return leafCert{}, false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return leafCert{}, false
	}
	if len(info.State.PeerCertificates) == 0 {
		return leafCert{}, false
	}
	return leafCert{Raw: info.State.PeerCertificates[0].Raw}, true
}

// leafCert is a tiny shim over *x509.Certificate so the interceptor
// only depends on the field it actually uses — the raw DER bytes the
// fingerprint hash is computed from.
type leafCert struct {
	Raw []byte
}

// bindingCache is a per-process TTL cache of (caller → fingerprint
// state). One DB read per CN per TTL window under burst — at our QPS
// floor this is the difference between "Postgres absorbs the load"
// and "Postgres becomes the bottleneck."
//
// Stale-while-revalidate is intentionally NOT implemented: the TTL is
// short (5s default) and rotation/revoke decisions are operator-paced
// — extra freshness machinery would be all cost, no benefit. Operators
// who need immediate effect can restart the server or wait one TTL.
type bindingCache struct {
	mu  sync.RWMutex
	m   map[string]bindingEntry
	ttl time.Duration
}

type bindingEntry struct {
	exists      bool
	fingerprint []byte
	expires     time.Time
}

func (c *bindingCache) lookup(ctx context.Context, caller string, fn CertBindingLookup) (bool, []byte, error) {
	now := time.Now()
	c.mu.RLock()
	e, ok := c.m[caller]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.exists, e.fingerprint, nil
	}
	exists, fp, err := fn(ctx, caller)
	if err != nil {
		return false, nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]bindingEntry)
	}
	c.m[caller] = bindingEntry{exists: exists, fingerprint: fp, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return exists, fp, nil
}
