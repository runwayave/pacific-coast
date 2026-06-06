package sandbox

import (
	"context"
	"sync"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Cache is the runtime.Cache stub the sim uses. It is NOT a real cache:
//
//   - Get always returns runtime.ErrCacheMiss
//   - Set silently accepts the value (test code can observe writes via
//     LastSet if it needs to)
//   - CurrentVersion returns 0 (which generated handlers treat as
//     "no pointer yet → miss")
//
// The contract this implementation pins is the one generated handlers
// depend on: every read miss falls through to the database (the sim),
// and every write is invisibly accepted. Real cache behavior — pointer
// indirection, single-flight, body-vs-pointer keys — does not affect
// correctness of the simulated server, only its production-shape
// performance. Tests that care about cache-hit paths must drive a real
// cache; that's an out-of-scope concern for sim.
//
// The stub IS thread-safe; LastSet snapshots are taken under the same
// mutex Set uses, so concurrent test code sees a consistent view.
type Cache struct {
	mu       sync.Mutex
	lastSets []SetCall
}

// SetCall is a recorded Cache.Set invocation. Tests use this to assert
// that codegen-emitted post-write cache-population code ran without
// having to stand up a real cache.
type SetCall struct {
	Key   string
	Value []byte
	TTL   time.Duration
	At    time.Time
}

// NewCache returns an empty Cache stub.
func NewCache() *Cache { return &Cache{} }

// Compile-time check.
var _ runtime.Cache = (*Cache)(nil)

// Get always returns ErrCacheMiss — the sim has no actual cache.
func (c *Cache) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, runtime.ErrCacheMiss
}

// Set silently records the write. Production code uses Cache to keep
// hot bodies warm; the sim doesn't need that, so we just remember the
// call for tests.
func (c *Cache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	c.lastSets = append(c.lastSets, SetCall{
		Key: key, Value: value, TTL: ttl, At: time.Now(),
	})
	c.mu.Unlock()
	return nil
}

// CurrentVersion returns 0 — the codegen treats that as "no pointer
// yet, miss the cache" and falls through to the DB. This is the
// correct behavior for the sim because there is no out-of-process
// version map.
func (c *Cache) CurrentVersion(_ context.Context, _, _ string) (int64, error) {
	return 0, nil
}

// LastSets returns a copy of recorded Set calls. Tests use it to
// confirm a handler called Set as expected.
func (c *Cache) LastSets() []SetCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SetCall, len(c.lastSets))
	copy(out, c.lastSets)
	return out
}

// Reset clears the recorded Set log. Helpful between sub-tests.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.lastSets = nil
	c.mu.Unlock()
}
