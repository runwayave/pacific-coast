// Package queryresult is the tier-2 cache: per-entity PK lists keyed by
// filter hash + generation counter. Counter at atl:v2:{entity}:gen.
package queryresult

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rachitkumar205/atlantis/internal/obs"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// MemcachedClient is the subset of the memcached client surface this
// package needs. Kept narrow so tests can implement a fake without
// dragging in the full *memcached.Client (which needs a real server).
type MemcachedClient interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Increment(ctx context.Context, key string, delta uint64) (int64, error)
	CounterValue(ctx context.Context, key string) (int64, error)
}

// Cache is the tier-2 query-result cache.
type Cache struct {
	mc MemcachedClient
}

// New returns a Cache that reads/writes tier-2 entries through mc.
func New(mc MemcachedClient) *Cache {
	return &Cache{mc: mc}
}

// Generation returns the current generation counter for entity. A counter
// that's never been bumped reads as 0; that's a stable starting point
// (any subsequent bump moves it to >= 1, retiring every cache entry
// hashed with gen=0).
func (c *Cache) Generation(ctx context.Context, entity string) (int64, error) {
	if c == nil {
		return 0, nil
	}
	v, err := c.mc.CounterValue(ctx, runtime.GenerationKey(entity))
	if err != nil {
		// Read failure: degrade to a "no gen recorded" answer. The hash
		// derivation still produces a stable key; we trade cache-hit
		// rate for availability when memcached is unreachable.
		return 0, nil
	}
	return v, nil
}

// BumpGeneration atomically increments the counter and returns the new
// value. Called by the outbox worker after debouncing.
func (c *Cache) BumpGeneration(ctx context.Context, entity string) (int64, error) {
	if c == nil {
		return 0, fmt.Errorf("queryresult: nil cache")
	}
	return c.mc.Increment(ctx, runtime.GenerationKey(entity), 1)
}

// Entry is one decoded tier-2 cache record: the PK list plus the
// keyset next_page_token to echo if the query was paginated. A
// cache-miss returns hit=false and a zero Entry; a corrupt value
// surfaces the decode error so monitoring can spot persistent damage.
type Entry struct {
	PKs           []string
	NextPageToken string
}

// Lookup checks tier 2 for a cached entry. Generated handlers should
// treat any error as a miss-equivalent (degrade to PG); the error is
// surfaced for telemetry, not for control flow.
//
// `hash` is the caller-supplied canonical-form digest from the Hash
// function. Generated handlers compute it once and pass it to both
// Lookup and Store.
func (c *Cache) Lookup(ctx context.Context, entity, hash string) (Entry, bool, error) {
	if c == nil {
		return Entry{}, false, nil
	}
	key := runtime.QueryResultKey(entity, hash)
	raw, err := c.mc.Get(ctx, key)
	if err != nil {
		obs.CacheTier2Misses.Inc()
		if errors.Is(err, runtime.ErrCacheMiss) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	p, err := decodePayload(raw)
	if err != nil {
		// Corrupt payload — treat as a miss; the caller falls through to
		// recompute. Recording as miss (not hit) keeps the hit-rate
		// metric honest.
		obs.CacheTier2Misses.Inc()
		return Entry{}, false, err
	}
	obs.CacheTier2Hits.Inc()
	return Entry{PKs: p.PKs, NextPageToken: p.NextPageToken}, true, nil
}

// Store writes an entry under the entity+hash key with the given TTL.
// TTL = 0 means "do not cache" — the call is a no-op. nextPageToken
// may be empty when the page was the last in its sequence.
func (c *Cache) Store(ctx context.Context, entity, hash string, pks []string, nextPageToken string, ttl time.Duration) error {
	if c == nil || ttl <= 0 {
		return nil
	}
	enc, err := encodePayload(payload{PKs: pks, NextPageToken: nextPageToken})
	if err != nil {
		return err
	}
	return c.mc.Set(ctx, runtime.QueryResultKey(entity, hash), enc, ttl)
}
