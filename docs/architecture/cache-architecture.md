# Cache architecture

Implementation of the two-cache model described in [Caching and invalidation](../concepts/caching-and-invalidation.md). Source lives under `internal/cache/{read,invalidate,memcached,queryresult}/`.

Atlantis runs two caches with different shapes:

- **Body cache** — per-row, keyed by `(namespace, entity, pk, version)`. Used by `Get` and includes.
- **Query-result cache** — per-filter, keyed by `(namespace, entity, generation, filter-hash)`. Used by `Query`.

Both share one memcached cluster and one transactional outbox.

## Storage layout

Every entry is namespaced. Four key shapes share the namespace:

```
body:<namespace>:<entity>:<pk>:v<version>     # body cache entry — immutable once written
pointer:<namespace>:<entity>:<pk>             # current version number for an entity row
query:<namespace>:<entity>:g<gen>:<hash>      # query-result cache entry
gen:<namespace>:<entity>                      # current generation counter for an entity
```

`body:...:v<version>` entries are immutable: once written under a version, the bytes never change. A write that updates a row stores a new body under a new version and updates the pointer separately. Old body entries become unreachable when the pointer moves past them; memcached LRU evicts them eventually.

## Body cache read path

A `GetEntity` for primary key `pk`:

1. Read `pointer:<namespace>:<entity>:<pk>` → returns version `v`.
2. Read `body:<namespace>:<entity>:<pk>:v<v>` → returns proto bytes if present.
3. On either miss, fall through to Postgres, store under a new version, update the pointer.

The pointer level adds one RTT per read on a cache hit. The win is that body entries are immutable: concurrent writers never race on the same key, and a stale body that no pointer references can be safely evicted by LRU without coordination.

## Body cache write path

A `Create`, `Update`, or `Delete`, inside the write transaction:

1. Update the entity's row in Postgres.
2. `INSERT INTO outbox (kind, namespace, entity, pk, ...)` — one row.
3. Commit.

The outbox row carries enough information for the worker to know which keys to act on. For an `Update`, the worker writes the new body under a new version and updates the pointer. For a `Delete`, the worker bumps the pointer to a tombstone version. The data write itself (step 1) does not touch memcached; all cache mutation runs through the worker.

**Transactional invariant.** Step 1 and step 2 commit atomically. There is no committed write without a queued invalidation, and no queued invalidation without a committed write.

## Query-result cache read path

A `Query` with filter `F`:

1. Read `gen:<namespace>:<entity>` → returns generation `g`.
2. Hash the canonical form of `F` (see [Filter hashing](#filter-hashing) below) to `h`.
3. Read `query:<namespace>:<entity>:g<g>:<h>` → returns the cached result if present.
4. On a miss, run the SQL, store under the same key, return.

The generation is in the key. After a write to the entity, the generation increments and the next read forms a key with a higher `g`, missing the cache and falling through to Postgres. Old-generation entries become unreachable; LRU evicts them.

## The outbox worker

A single goroutine in the server polls the `outbox` table at `OUTBOX_DRAIN_INTERVAL` (default `250ms`):

```sql
SELECT id, kind, payload FROM outbox WHERE done_at IS NULL ORDER BY id LIMIT $batch
```

For each row, it applies the invalidation to memcached and marks the row done. Single-instance is currently enforced at the deployment level — running multiple server replicas with `OUTBOX_BATCH_SIZE > 0` would have them compete on the same rows. A future version may add `FOR UPDATE SKIP LOCKED` and shard work by entity.

## Filter hashing

The query-result cache hashes the typed filter expression after a two-phase normalization:

**Syntactic normalization** — guarantees the same canonical byte representation for equivalent expressions:

- Predicates are sorted by proto field number.
- Lists are sorted.

**Semantic rewrites** — collapse logically-equivalent expressions to one form. These are safe because the rewrites preserve the truth value:

- Empty AND/OR/NOT branches are removed (`AND []` is the identity).
- Equal pairs are coalesced (`x=1 AND x=1` becomes `x=1`).

The hash is `SHA-256` truncated to the first 16 bytes, base32-encoded.

## Failure modes

**Stale-read window after a write.** Between commit and worker pickup, a reader can fetch `pointer=v7`, the worker bumps to `v8` and writes the new body, the reader fetches `body:...:v7` and gets a stale-but-self-consistent result. The window is bounded by `OUTBOX_DRAIN_INTERVAL`.

**Cross-key non-atomicity.** Memcached has no cross-key transaction. Within the body cache, a reader observing `(pointer=v7, body=v7-bytes)` is consistent (both are the v7 snapshot); the only way to observe inconsistency is to be in the window above.

**Server crash mid-invalidation.** Outbox rows are in Postgres. A crash after commit but before the worker marks a row done leaves the row pending; the next worker pass picks it up.

**Memcached restart.** Cold cache. Reads cold-miss to Postgres and re-populate. No correctness issue; latency spikes until the working set re-warms.

## Tradeoffs

- One extra RTT per body-cache read (pointer lookup).
- A stale-read window bounded by `OUTBOX_DRAIN_INTERVAL`.
- Query-cache entries under old generations occupy memcached until LRU evicts them.

Body entries are immutable, so concurrent readers and writers do not race on the same key. The outbox row records every pending invalidation in Postgres, so a server or worker crash does not lose them. The invalidation pipeline is per-entity-generic; adding an entity does not require new cache code.
