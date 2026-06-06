// Package interceptors carries the gRPC server-side interceptors that run
// alongside the codegen entity handlers — per-caller rate limits today,
// per-RPC metrics + tracing next.
//
// Each interceptor is built as a constructor returning a
// grpc.UnaryServerInterceptor so callers can wire them into
// grpc.ChainUnaryInterceptor in whatever order the policy demands.
package interceptors

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RateLimitConfig parameterises the token-bucket interceptor. Values come
// from the server's config layer (loadConfig). The defaults (1000 qps,
// 200 burst, 0.80 saturation cutoff) are loose enough that an
// unconfigured single-tenant deployment never sheds on its own load —
// operators dial them down per workload.
type RateLimitConfig struct {
	// DefaultQPS is the per-caller token-bucket refill rate when no
	// per-caller override is set; overrides via RateLimitConfig.PerCaller.
	DefaultQPS int
	// Burst is the max tokens the bucket can accumulate; lets a brief
	// burst exceed DefaultQPS as long as the average rate stays under.
	Burst int
	// PerCaller overrides the default for specific callers (matched by
	// the resolved x-caller / mTLS-CN identity).
	PerCaller map[string]int
	// PoolSaturationCutoff is the fraction of MaxConns at which load
	// shedding for low-priority RPCs kicks in. 0.80 leaves headroom for
	// write-shaped traffic when the pool is hot.
	PoolSaturationCutoff float64
	// CallerFromContext extracts the caller identity from ctx. Wired to
	// cmd/server/auth.go:callerFromContext at construction; broken out
	// here so tests can inject a fake without importing cmd/server.
	CallerFromContext func(context.Context) string
}

// withDefaults returns c with zero-valued fields replaced by the
// production defaults documented on RateLimitConfig.
func (c RateLimitConfig) withDefaults() RateLimitConfig {
	if c.DefaultQPS <= 0 {
		c.DefaultQPS = 1000
	}
	if c.Burst <= 0 {
		c.Burst = 200
	}
	if c.PoolSaturationCutoff <= 0 {
		c.PoolSaturationCutoff = 0.80
	}
	if c.CallerFromContext == nil {
		c.CallerFromContext = func(context.Context) string { return "anonymous" }
	}
	return c
}

// NewRateLimit wires a token-bucket-per-caller interceptor. Token buckets
// are created lazily on first request from a caller; the goroutine-safe
// map carries them indefinitely (callers are a low-cardinality set —
// today's tally is `backend`, `vendor-platform`, future analytics / ML).
//
// Load shedding policy:
//   - When the pgxpool is below the saturation cutoff, every request
//     consumes 1 token and only excess traffic is dropped with
//     ResourceExhausted.
//   - At/above the cutoff, requests that match isLowPriority are dropped
//     unconditionally so the pool stays reserved for write-shaped
//     traffic. Classification is hard-coded today: any RPC whose method
//     name starts with `List` or `Search` is low priority; CRUD and Get
//     never shed via this gate. A future DSL extension for per-RPC
//     priority annotations would make this table-driven.
//
// The `pool` argument is the pgx pool the server uses for entity reads.
// We never *call* the pool here — only inspect Stat() to decide whether
// to shed. A nil pool disables saturation-aware shedding (the bucket
// limit still applies).
func NewRateLimit(pool *pgxpool.Pool, cfg RateLimitConfig) grpc.UnaryServerInterceptor {
	cfg = cfg.withDefaults()
	buckets := newBucketRegistry(cfg)

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		caller := cfg.CallerFromContext(ctx)

		// pool.Stat() is safe on closed pools (returns zeros) and on healthy
		// ones, but wrap defensively — interceptors run before
		// recoveryInterceptor in tests / future refactors. A panic here
		// would crash the gRPC handshake without surfacing diagnostics.
		if pool != nil && isLowPriority(info.FullMethod) {
			if ratio, ok := saturationRatio(pool); ok && ratio >= cfg.PoolSaturationCutoff {
				return nil, status.Errorf(codes.ResourceExhausted,
					"atlantis: low-priority RPC shed at %.0f%% pool saturation",
					ratio*100)
			}
		}

		if !buckets.take(caller) {
			return nil, status.Errorf(codes.ResourceExhausted,
				"atlantis: caller %q exceeded rate limit", caller)
		}
		return handler(ctx, req)
	}
}

// saturationRatio computes acquired/max for the pool. Returns (0, false)
// for a nil pool, a zero-max pool, or anything else that would make the
// ratio meaningless — callers treat that as "shedding disabled, don't
// drop the RPC."
func saturationRatio(pool *pgxpool.Pool) (float64, bool) {
	if pool == nil {
		return 0, false
	}
	st := pool.Stat()
	max := st.MaxConns()
	if max <= 0 {
		return 0, false
	}
	return float64(st.AcquiredConns()) / float64(max), true
}

// isLowPriority classifies an RPC's full method path as low-priority for
// load shedding. The DSL does not yet carry per-RPC priority annotations,
// so today the rule is "list/search are low, Get and the mutators are
// high."
//
// The string match is on the *method* component of the gRPC path:
// "/atlantis.v1.<entity>.<Service>/<Method>" → we look at
// what comes after the final "/".
func isLowPriority(fullMethod string) bool {
	i := strings.LastIndex(fullMethod, "/")
	if i < 0 || i == len(fullMethod)-1 {
		return false
	}
	name := fullMethod[i+1:]
	return strings.HasPrefix(name, "List") || strings.HasPrefix(name, "Search")
}

// bucketRegistry is a goroutine-safe map of caller → token bucket. It's
// the simplest correct shape: a sync.RWMutex around a map keyed by caller
// name. Cardinality is tiny (a handful of internal services), so we don't
// bother with sharded maps.
type bucketRegistry struct {
	cfg     RateLimitConfig
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

func newBucketRegistry(cfg RateLimitConfig) *bucketRegistry {
	return &bucketRegistry{cfg: cfg, buckets: map[string]*tokenBucket{}}
}

func (r *bucketRegistry) take(caller string) bool {
	r.mu.Lock()
	b, ok := r.buckets[caller]
	if !ok {
		qps := r.cfg.DefaultQPS
		if override, ok := r.cfg.PerCaller[caller]; ok && override > 0 {
			qps = override
		}
		b = newTokenBucket(qps, r.cfg.Burst)
		r.buckets[caller] = b
	}
	r.mu.Unlock()
	return b.take()
}

// tokenBucket is a fixed-rate token bucket. Refills at qps tokens/sec up
// to burst; take() consumes one. The math is a single multiplication on
// each call — cheaper than maintaining a goroutine that ticks.
//
// Refill formula: the bucket holds at most `burst` tokens and refills at
// `qps` tokens per second. We track "tokens available as of lastRefill"
// rather than running a goroutine ticker — the lazy compute is cache-
// friendly and avoids per-bucket goroutine overhead.
type tokenBucket struct {
	mu         sync.Mutex
	qps        float64
	burst      float64
	tokens     float64
	lastRefill time.Time
}

func newTokenBucket(qps, burst int) *tokenBucket {
	return &tokenBucket{
		qps:        float64(qps),
		burst:      float64(burst),
		tokens:     float64(burst),
		lastRefill: time.Now(),
	}
}

func (b *tokenBucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.qps
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.lastRefill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
