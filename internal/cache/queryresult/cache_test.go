package queryresult

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"

	commonv1 "github.com/rachitkumar205/atlantis-go/pb/atlantis/common/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fakeMC is an in-memory MemcachedClient. Concurrency-safe so worker
// tests can exercise it; not a benchmark target.
type fakeMC struct {
	mu       sync.Mutex
	store    map[string][]byte
	counters map[string]int64
	getErr   error // injected for Lookup-failure tests
}

func newFakeMC() *fakeMC {
	return &fakeMC{store: map[string][]byte{}, counters: map[string]int64{}}
}

func (f *fakeMC) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	v, ok := f.store[key]
	if !ok {
		return nil, runtime.ErrCacheMiss
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

func (f *fakeMC) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(value))
	copy(cp, value)
	f.store[key] = cp
	return nil
}

func (f *fakeMC) Increment(_ context.Context, key string, delta uint64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters[key] += int64(delta)
	return f.counters[key], nil
}

func (f *fakeMC) CounterValue(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters[key], nil
}

// ---------------------------------------------------------------------------
// Cache: Lookup / Store

func TestCache_StoreThenLookup_RoundTrips(t *testing.T) {
	mc := newFakeMC()
	c := New(mc)
	ctx := context.Background()

	pks := []string{"a", "b", "c"}
	if err := c.Store(ctx, "consumer.Account", "hash1", pks, "", time.Minute); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, hit, err := c.Lookup(ctx, "consumer.Account", "hash1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !hit {
		t.Fatalf("expected hit")
	}
	if len(got.PKs) != 3 || got.PKs[0] != "a" || got.PKs[1] != "b" || got.PKs[2] != "c" {
		t.Errorf("round-trip: got %v", got.PKs)
	}
	if got.NextPageToken != "" {
		t.Errorf("empty token stored; got %q", got.NextPageToken)
	}
}

func TestCache_StoreThenLookup_NextPageTokenRoundTrips(t *testing.T) {
	mc := newFakeMC()
	c := New(mc)
	ctx := context.Background()

	if err := c.Store(ctx, "e", "h", []string{"x"}, "cursor-bytes-here", time.Minute); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, hit, err := c.Lookup(ctx, "e", "h")
	if err != nil || !hit {
		t.Fatalf("Lookup: hit=%v err=%v", hit, err)
	}
	if got.NextPageToken != "cursor-bytes-here" {
		t.Errorf("token round-trip: got %q", got.NextPageToken)
	}
}

func TestCache_Lookup_Miss(t *testing.T) {
	c := New(newFakeMC())
	got, hit, err := c.Lookup(context.Background(), "consumer.Account", "absent")
	if err != nil {
		t.Fatalf("Lookup miss should not error: %v", err)
	}
	if hit {
		t.Errorf("expected miss")
	}
	if len(got.PKs) != 0 || got.NextPageToken != "" {
		t.Errorf("expected zero Entry on miss, got %+v", got)
	}
}

func TestCache_Store_TTLZeroIsNoop(t *testing.T) {
	// `read_uncached` entities pass TTL=0 → Store is a no-op.
	mc := newFakeMC()
	c := New(mc)
	if err := c.Store(context.Background(), "x", "h", []string{"a"}, "", 0); err != nil {
		t.Fatalf("Store TTL=0: %v", err)
	}
	if len(mc.store) != 0 {
		t.Errorf("TTL=0 should not store; got %d entries", len(mc.store))
	}
}

func TestCache_Lookup_CorruptValueReportsError(t *testing.T) {
	// A memcached-network attacker (or bit-flip) substitutes a value with
	// an unknown codec version. The reader treats it as a miss but
	// surfaces the decode error so monitoring can flag persistent
	// corruption.
	mc := newFakeMC()
	c := New(mc)
	mc.store[runtime.QueryResultKey("e", "h")] = []byte{0xFF, 0x01, 'a'}
	got, hit, err := c.Lookup(context.Background(), "e", "h")
	if err == nil {
		t.Errorf("expected decode error")
	}
	if hit {
		t.Errorf("corrupt value should not hit")
	}
	if len(got.PKs) != 0 {
		t.Errorf("expected zero Entry, got %+v", got)
	}
}

func TestCache_Lookup_InfraErrorSurfaced(t *testing.T) {
	mc := newFakeMC()
	mc.getErr = errors.New("network down")
	c := New(mc)
	_, hit, err := c.Lookup(context.Background(), "e", "h")
	if err == nil {
		t.Errorf("expected infra error to surface")
	}
	if hit {
		t.Errorf("infra error should not hit")
	}
}

// ---------------------------------------------------------------------------
// Cache: Generation counter

func TestCache_Generation_StartsAtZero(t *testing.T) {
	c := New(newFakeMC())
	v, err := c.Generation(context.Background(), "consumer.Account")
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if v != 0 {
		t.Errorf("initial gen = %d, want 0", v)
	}
}

func TestCache_BumpGeneration_IncrementsAtomically(t *testing.T) {
	c := New(newFakeMC())
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		got, err := c.BumpGeneration(ctx, "consumer.Account")
		if err != nil {
			t.Fatalf("Bump: %v", err)
		}
		if got != i {
			t.Errorf("bump %d = %d, want %d", i, got, i)
		}
	}
	v, _ := c.Generation(ctx, "consumer.Account")
	if v != 5 {
		t.Errorf("post-bumps Generation = %d, want 5", v)
	}
}

func TestCache_Generation_PerEntityIsolated(t *testing.T) {
	c := New(newFakeMC())
	ctx := context.Background()
	_, _ = c.BumpGeneration(ctx, "consumer.Account")
	_, _ = c.BumpGeneration(ctx, "consumer.Account")
	_, _ = c.BumpGeneration(ctx, "vendor.Product")
	a, _ := c.Generation(ctx, "consumer.Account")
	p, _ := c.Generation(ctx, "vendor.Product")
	if a != 2 {
		t.Errorf("Account gen = %d, want 2", a)
	}
	if p != 1 {
		t.Errorf("Product gen = %d, want 1", p)
	}
}

// ---------------------------------------------------------------------------
// Codec

func TestCodec_V2RoundTrips(t *testing.T) {
	cases := []payload{
		{},
		{PKs: []string{}},
		{PKs: []string{"a"}},
		{PKs: []string{"a", "b", "c"}},
		{PKs: []string{"", "x"}}, // empty pk in list
		{PKs: []string{string(make([]byte, 128))}},         // 128-byte pk
		{PKs: []string{"a", "b"}, NextPageToken: "cur123"}, // both fields
		{NextPageToken: "only-token"},                      // token, no rows
	}
	for i, c := range cases {
		enc, err := encodePayload(c)
		if err != nil {
			t.Fatalf("case %d encode: %v", i, err)
		}
		got, err := decodePayload(enc)
		if err != nil {
			t.Fatalf("case %d decode: %v", i, err)
		}
		if len(got.PKs) != len(c.PKs) {
			t.Errorf("case %d: pk len = %d, want %d", i, len(got.PKs), len(c.PKs))
			continue
		}
		for j := range c.PKs {
			if got.PKs[j] != c.PKs[j] {
				t.Errorf("case %d entry %d: %q vs %q", i, j, got.PKs[j], c.PKs[j])
			}
		}
		if got.NextPageToken != c.NextPageToken {
			t.Errorf("case %d token: %q vs %q", i, got.NextPageToken, c.NextPageToken)
		}
	}
}

func TestCodec_V1BackwardCompat(t *testing.T) {
	// Hand-craft a v1 payload: [0x01][uvarint(1)]['a']
	v1 := []byte{codecVersion1, 0x01, 'a'}
	got, err := decodePayload(v1)
	if err != nil {
		t.Fatalf("v1 decode: %v", err)
	}
	if len(got.PKs) != 1 || got.PKs[0] != "a" {
		t.Errorf("v1 pks: got %v", got.PKs)
	}
	if got.NextPageToken != "" {
		t.Errorf("v1 token should be empty, got %q", got.NextPageToken)
	}
}

func TestCodec_RejectsEmpty(t *testing.T) {
	if _, err := decodePayload(nil); err == nil {
		t.Errorf("expected error on empty input")
	}
}

func TestCodec_RejectsUnknownVersion(t *testing.T) {
	if _, err := decodePayload([]byte{0xFE}); err == nil {
		t.Errorf("expected error on unknown version")
	}
}

func TestCodec_RejectsTruncated(t *testing.T) {
	enc, _ := encodePayload(payload{PKs: []string{"hello"}})
	if _, err := decodePayload(enc[:len(enc)-2]); err == nil {
		t.Errorf("expected truncation error")
	}
}

func TestCodec_RejectsOversizedToken(t *testing.T) {
	huge := string(make([]byte, MaxNextPageTokenLen+1))
	if _, err := encodePayload(payload{NextPageToken: huge}); err == nil {
		t.Errorf("expected error on oversized token")
	}
}

// ---------------------------------------------------------------------------
// Hash

func TestHash_DeterministicAcrossCalls(t *testing.T) {
	f := &commonv1.StringPredicate{Op: &commonv1.StringPredicate_Eq{Eq: "alice"}}
	h1, err := Hash("consumer.Account", f, nil, 100, nil, nil, []int32{1, 2}, 7)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h2, _ := Hash("consumer.Account", f, nil, 100, nil, nil, []int32{1, 2}, 7)
	if h1 != h2 {
		t.Errorf("hashes diverged: %q vs %q", h1, h2)
	}
	if len(h1) != 22 {
		t.Errorf("hash len = %d, want 22 (base64url no-pad of 16 bytes)", len(h1))
	}
}

func TestHash_IncludesOrderInsensitive(t *testing.T) {
	// `includes` is a Set conceptually — canonical form sorts before hash.
	a, _ := Hash("e", nil, nil, 0, nil, nil, []int32{3, 1, 2}, 0)
	b, _ := Hash("e", nil, nil, 0, nil, nil, []int32{1, 2, 3}, 0)
	if a != b {
		t.Errorf("includes order should be canonicalized: %q vs %q", a, b)
	}
}

func TestHash_DifferentGenerationsProduceDifferentHashes(t *testing.T) {
	a, _ := Hash("e", nil, nil, 0, nil, nil, nil, 0)
	b, _ := Hash("e", nil, nil, 0, nil, nil, nil, 1)
	if a == b {
		t.Errorf("generation bump should change hash")
	}
}

func TestHash_DifferentEntitiesProduceDifferentHashes(t *testing.T) {
	a, _ := Hash("consumer.Account", nil, nil, 0, nil, nil, nil, 0)
	b, _ := Hash("vendor.Product", nil, nil, 0, nil, nil, nil, 0)
	if a == b {
		t.Errorf("different entities should differ in hash")
	}
}

func TestHash_NilFilterStable(t *testing.T) {
	// Two callers both omitting filter → same hash.
	a, _ := Hash("e", nil, nil, 100, nil, nil, nil, 0)
	b, _ := Hash("e", nil, nil, 100, nil, nil, nil, 0)
	if a != b {
		t.Errorf("nil filter not stable")
	}
}

func TestHash_FieldMaskAffectsHash(t *testing.T) {
	m1 := &fieldmaskpb.FieldMask{Paths: []string{"id"}}
	m2 := &fieldmaskpb.FieldMask{Paths: []string{"id", "name"}}
	a, _ := Hash("e", nil, nil, 0, nil, m1, nil, 0)
	b, _ := Hash("e", nil, nil, 0, nil, m2, nil, 0)
	if a == b {
		t.Errorf("different field masks should differ in hash")
	}
}

func TestHash_OrderOrderMatters(t *testing.T) {
	// Unlike `includes`, `orders` is semantically ordered — `ORDER BY a, b`
	// is not the same as `ORDER BY b, a`.
	o1 := &commonv1.Int32Range{Lo: 1, Hi: 2} // any proto message will do as a stand-in
	o2 := &commonv1.Int32Range{Lo: 3, Hi: 4}
	a, _ := Hash("e", nil, []proto.Message{o1, o2}, 0, nil, nil, nil, 0)
	b, _ := Hash("e", nil, []proto.Message{o2, o1}, 0, nil, nil, nil, 0)
	if a == b {
		t.Errorf("orders order should affect hash")
	}
}
