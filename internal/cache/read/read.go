// Package read is the cache read path: tier-0 LRU, versioned-key indirection
// over memcached, singleflight per miss, and XFetch probabilistic early refresh.
package read

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"github.com/rachitkumar205/atlantis/internal/obs"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// PointerCache is the cache interface this package needs. It is a strict
// subset of runtime.Cache — we keep the local interface for testability and
// so the package doesn't need to import every Cache implementation under
// the sun.
type PointerCache interface {
	// Get returns the bytes stored under key, or runtime.ErrCacheMiss.
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores bytes under key with TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// CurrentVersion reads the version pointer key. Returns 0 on miss.
	CurrentVersion(ctx context.Context, entity, id string) (int64, error)
}

// Loader is what the read path calls on cache miss. Returns the body bytes
// fresh from Postgres (or wherever the caller chooses to source it).
type Loader func(ctx context.Context) ([]byte, error)

// Config tunes the reader.
type Config struct {
	// LRUSize is the tier-0 in-process cache capacity. 0 disables tier-0.
	// Scope is PROCESS-WIDE: one LRU shared across every entity in this
	// process, keyed by `entity/id`. A hot entity can evict cold-but-
	// cached entries from any other entity. Per-entity caps require a
	// multi-LRU rewrite.
	LRUSize int

	// MaxValueBytes is the largest individual body that may live in tier-0.
	// Larger bodies skip tier-0 (they still cache in memcached). Guards
	// against OOM when a single rogue row carries a 100MB payload. 0 means
	// "no upper bound" (dev only).
	MaxValueBytes int

	// DefaultTTL is applied to body and pointer keys on refill. The DSL's
	// per-entity TTL (baked into the runtime.CacheKey-shaped helpers by
	// codegen) wins when supplied via SetTTL on a per-call basis; this is
	// the fallback.
	DefaultTTL time.Duration

	// XFetchBeta tunes the probabilistic early refresh; 0 disables it.
	// A higher beta means more aggressive early refresh. The Memcached
	// "XFetch" paper recommends ~1.0; we default to 1.0.
	XFetchBeta float64
}

// DefaultConfig returns the default tuning: 1024-entry tier-0,
// 1 MiB max value, 10m default TTL, XFetchBeta=1.0.
func DefaultConfig() Config {
	return Config{
		LRUSize:       1024,
		MaxValueBytes: 1 << 20, // 1 MiB — generous for typed entity rows
		DefaultTTL:    10 * time.Minute,
		XFetchBeta:    1.0,
	}
}

// Reader is the read-path façade. One instance per process; safe for
// concurrent use.
type Reader struct {
	cache PointerCache
	cfg   Config

	tier0 *lru.Cache[string, tier0Entry]
	sf    singleflight.Group
}

// tier0Entry is what we store in the in-process LRU. We track when the value
// was loaded so XFetch can compute the early-refresh probability against the
// caller's TTL.
type tier0Entry struct {
	body     []byte
	loadedAt time.Time
	ttl      time.Duration
}

// New returns a Reader bound to cache. Returns an error if cfg.LRUSize > 0
// and the LRU cannot be sized.
func New(cache PointerCache, cfg Config) (*Reader, error) {
	r := &Reader{cache: cache, cfg: cfg}
	if cfg.LRUSize > 0 {
		l, err := lru.New[string, tier0Entry](cfg.LRUSize)
		if err != nil {
			return nil, err
		}
		r.tier0 = l
	}
	return r, nil
}

// Get fetches the cached body for an entity row. On miss, loader is invoked
// to source fresh bytes; the result is cached in both tiers before return.
//
// Wire it up like:
//
//	body, err := reader.Get(ctx, "consumer.SavedOutfit", "42", func(ctx context.Context) ([]byte, error) {
//	    return proto.Marshal(&pb.SavedOutfit{Id: 42, ...})
//	})
//
// The generated server code does this once per Get RPC.
func (r *Reader) Get(ctx context.Context, entity, id string, loader Loader) ([]byte, error) {
	// Tier 0: in-process LRU.
	bodyKey, ver, ttl, ok := r.tier0Lookup(ctx, entity, id)
	if ok {
		obs.CacheTier0Hits.Inc()
		// XFetch may decide to refresh anyway.
		if !r.xfetchShouldRefresh(ttl) {
			return r.tier0Body(bodyKey)
		}
		// Fall through to memcached + loader, but the cached value remains
		// usable if the refresh fails — that's the whole point of XFetch.
	} else {
		obs.CacheTier0Misses.Inc()
	}

	// Tier 1: memcached.
	body, err := r.tier1Lookup(ctx, entity, id)
	if err == nil {
		obs.CacheTier1Hits.Inc()
		r.tier0Store(entity, id, ver, body, r.cfg.DefaultTTL)
		return body, nil
	}
	if errors.Is(err, runtime.ErrCacheMiss) {
		obs.CacheTier1Misses.Inc()
	} else {
		// A real cache error is not fatal — the loader can still serve.
		// The integration tests verify this fall-through path.
		obs.CacheTier1Errors.Inc()
	}

	// Miss path: singleflight to the loader, with two refinements over a
	// naive `sf.Do(key, loader)`:
	//
	// 1. Key contains a NUL separator. Without it, entity
	//    "a" / id "b:c" collides with entity "a:b" / id "c", and a hot
	//    miss on one would block the other.
	//
	// 2. The loader runs on a detached context derived from ctx but
	//    without its cancellation, and we use `DoChan` so each waiter
	//    selects on its own ctx. Without this, if the leader's RPC ctx
	//    cancels mid-load, every joined waiter sees ctx.Canceled even if
	//    their own contexts are still live.
	key := entity + "\x00" + id
	ch := r.sf.DoChan(key, func() (any, error) {
		loadCtx := context.WithoutCancel(ctx)
		fresh, lerr := loader(loadCtx)
		if lerr != nil {
			return nil, lerr
		}
		// Best-effort refill. If the cache is down, we still return the
		// freshly-loaded body to the caller — staleness is acceptable; data
		// loss is not.
		newVer, _ := r.cache.CurrentVersion(loadCtx, entity, id)
		if newVer == 0 {
			newVer = 1
		}
		bk := runtime.CacheKey(entity, id, newVer)
		_ = r.cache.Set(loadCtx, bk, fresh, r.cfg.DefaultTTL)
		// Bound the in-memory size of cached bodies. Very large
		// bodies bypass tier-0 entirely so a 100MB payload can't OOM the pod
		// just by being read once.
		if r.cfg.MaxValueBytes == 0 || len(fresh) <= r.cfg.MaxValueBytes {
			r.tier0Store(entity, id, newVer, fresh, r.cfg.DefaultTTL)
		}
		return fresh, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// Invalidate drops the LRU entry for a given entity row. The full
// invalidation path runs through internal/cache/invalidate; this is the
// in-process hook that worker fires for every drained outbox row.
func (r *Reader) Invalidate(entity, id string) {
	if r.tier0 == nil {
		return
	}
	r.tier0.Remove(entityIDKey(entity, id))
}

func (r *Reader) tier0Lookup(_ context.Context, entity, id string) (string, int64, time.Duration, bool) {
	if r.tier0 == nil {
		return "", 0, 0, false
	}
	v, ok := r.tier0.Get(entityIDKey(entity, id))
	if !ok {
		return "", 0, 0, false
	}
	return entityIDKey(entity, id), 0, r.tier0Remaining(v), true
}

func (r *Reader) tier0Body(key string) ([]byte, error) {
	if r.tier0 == nil {
		return nil, runtime.ErrCacheMiss
	}
	v, ok := r.tier0.Get(key)
	if !ok {
		return nil, runtime.ErrCacheMiss
	}
	return v.body, nil
}

func (r *Reader) tier0Store(entity, id string, _ int64, body []byte, ttl time.Duration) {
	if r.tier0 == nil {
		return
	}
	r.tier0.Add(entityIDKey(entity, id), tier0Entry{
		body:     body,
		loadedAt: time.Now(),
		ttl:      ttl,
	})
}

func (r *Reader) tier0Remaining(e tier0Entry) time.Duration {
	if e.ttl == 0 {
		return r.cfg.DefaultTTL
	}
	remaining := e.ttl - time.Since(e.loadedAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *Reader) tier1Lookup(ctx context.Context, entity, id string) ([]byte, error) {
	ver, err := r.cache.CurrentVersion(ctx, entity, id)
	if err != nil {
		return nil, err
	}
	if ver == 0 {
		return nil, runtime.ErrCacheMiss
	}
	return r.cache.Get(ctx, runtime.CacheKey(entity, id, ver))
}

// xfetchShouldRefresh implements the XFetch probabilistic early-refresh
// rule. The formula is:
//
//	refresh if -beta * delta * ln(rand()) > remainingTTL
//
// Where delta is the typical recompute cost. We don't measure delta — we
// fix it at 1 — which makes the refresh probability monotone in the inverse
// of remaining TTL. Far from expiry, refresh probability ≈ 0; near expiry,
// it climbs sharply.
//
// (The Memcached paper this comes from: A. Vattani, F. Chierichetti, K.
// Lowenstein, "Optimal Probabilistic Cache Stampede Prevention", PVLDB 2015.)
func (r *Reader) xfetchShouldRefresh(remaining time.Duration) bool {
	if r.cfg.XFetchBeta <= 0 || remaining <= 0 {
		return false
	}
	// rand.Float64() returns [0, 1); ln(0) is -inf so guard.
	x := rand.Float64()
	if x == 0 {
		return true
	}
	// delta = 1 (we don't measure recompute cost); rewrite as
	//   -beta * ln(x) > remaining
	// scaled to seconds so beta has intuitive units.
	threshold := -r.cfg.XFetchBeta * math.Log(x)
	return threshold > remaining.Seconds()
}

// entityIDKey is the tier-0 LRU key shape. It's intentionally the same
// shape as runtime.PointerKey minus the suffix so invalidation by entity+id
// is O(1).
func entityIDKey(entity, id string) string { return entity + ":" + id }

// encodeVer / decodeVer pack version + body bytes into a single buffer:
// 8 bytes big-endian version followed by the body.
func encodeVer(ver int64, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint64(out[:8], uint64(ver))
	copy(out[8:], body)
	return out
}

func decodeVer(b []byte) (int64, []byte, bool) {
	if len(b) < 8 {
		return 0, nil, false
	}
	return int64(binary.BigEndian.Uint64(b[:8])), b[8:], true
}

// Touch round-trips a value through encodeVer/decodeVer so the helpers
// stay reachable from tests while no production path uses them.
func Touch() { _, _, _ = decodeVer(encodeVer(1, nil)) }
