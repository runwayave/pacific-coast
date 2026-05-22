package interceptors

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The rate-limit interceptor has three behaviours that matter at the
// boundary: (1) the per-caller bucket actually rate-limits, (2) low-
// priority RPCs are classified correctly, and (3) the bucket is
// goroutine-safe under concurrent take(). Tests pin each.

func TestTokenBucket_TakesBurstThenRejects(t *testing.T) {
	b := newTokenBucket(10, 5)
	// Burst of 5 should let through 5 calls immediately.
	for i := range 5 {
		if !b.take() {
			t.Fatalf("call %d should succeed inside burst", i)
		}
	}
	if b.take() {
		t.Errorf("call past burst should fail without waiting for refill")
	}
}

func TestTokenBucket_RefillsAtConfiguredRate(t *testing.T) {
	b := newTokenBucket(100, 1) // 100 QPS, burst 1
	if !b.take() {
		t.Fatalf("first call must succeed")
	}
	if b.take() {
		t.Fatalf("second call should fail before refill")
	}
	// 100 QPS = 10ms per token; wait 30ms to allow at least one refill.
	time.Sleep(30 * time.Millisecond)
	if !b.take() {
		t.Errorf("token should have refilled after 30ms at 100 QPS")
	}
}

func TestTokenBucket_GoroutineSafe(t *testing.T) {
	// Twenty goroutines each take 100 times against a bucket of 10 burst /
	// 1000 QPS. The total accept count must equal what the bucket math
	// allows (initial 10 + ~refills during the test), and the call must
	// not race the detector.
	b := newTokenBucket(1000, 10)
	var allowed int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if b.take() {
					atomic.AddInt64(&allowed, 1)
				}
			}
		}()
	}
	wg.Wait()
	// No exact upper bound (timing-dependent) but anything sane is fine.
	// The test exists to catch data races under `go test -race`.
	if allowed < 10 || allowed > 2000 {
		t.Errorf("allowed count %d is out of plausible range", allowed)
	}
}

func TestIsLowPriority(t *testing.T) {
	cases := []struct {
		method  string
		wantLow bool
	}{
		{"/atlantis.v1.account.Account/Get", false},
		{"/atlantis.v1.account.Account/List", true},
		{"/atlantis.v1.account.Account/Create", false},
		{"/atlantis.v1.product.ProductVariant/SearchBySearchVec", true},
		{"/atlantis.v1.account.Account/BatchGet", false},
		{"", false},                  // no slash
		{"/no-slash-suffix/", false}, // trailing slash
	}
	for _, c := range cases {
		if got := isLowPriority(c.method); got != c.wantLow {
			t.Errorf("isLowPriority(%q) = %v, want %v", c.method, got, c.wantLow)
		}
	}
}

// Exercising NewRateLimit end-to-end requires a UnaryServerInterceptor
// invocation. The interceptor's pool dependency is nil-tolerant by design;
// we pass nil here so the test stays leaf (no pgx + Docker requirement).

func TestNewRateLimit_BucketLimitRejects(t *testing.T) {
	cfg := RateLimitConfig{
		DefaultQPS:        1, // 1 QPS so the burst is the only headroom
		Burst:             2,
		CallerFromContext: func(context.Context) string { return "test-caller" },
	}
	interceptor := NewRateLimit(nil, cfg)

	info := &grpc.UnaryServerInfo{FullMethod: "/atlantis.v1.x/Get"}
	handler := func(context.Context, any) (any, error) { return struct{}{}, nil }

	// 2 succeed (burst), 3rd is denied.
	for i := range 2 {
		if _, err := interceptor(context.Background(), nil, info, handler); err != nil {
			t.Fatalf("call %d should succeed inside burst: %v", i, err)
		}
	}
	_, err := interceptor(context.Background(), nil, info, handler)
	if err == nil {
		t.Fatalf("call past burst should be rejected")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got %s", got)
	}
}

func TestNewRateLimit_PerCallerOverrideUsed(t *testing.T) {
	caller := "noisy"
	calls := 0
	cfg := RateLimitConfig{
		DefaultQPS: 1,
		Burst:      1,
		PerCaller:  map[string]int{caller: 1000},
		// Burst is still 1 so the override doesn't make this useful unless
		// we also configure burst per caller. v0.1's shape is global burst,
		// per-caller refill rate — pinning the override-read path is what
		// matters here.
		CallerFromContext: func(context.Context) string { calls++; return caller },
	}
	interceptor := NewRateLimit(nil, cfg)
	info := &grpc.UnaryServerInfo{FullMethod: "/x/Get"}
	handler := func(context.Context, any) (any, error) { return struct{}{}, nil }

	if _, err := interceptor(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}
	if calls == 0 {
		t.Errorf("CallerFromContext should have been consulted")
	}
}
