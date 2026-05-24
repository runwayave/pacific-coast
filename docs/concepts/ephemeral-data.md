# Ephemeral data

An `ephemeral` declaration is a typed data shape backed by memcached, not Postgres. No table, no migration, no VACUUM. The data lives in the cache layer with a declared TTL and is automatically evicted when the TTL expires.

```atl
ephemeral EphemeralOutfit in consumer {
  key           varchar(255) primary
  variant_ids   []text not null
  product_ids   []text not null
  user_item_ids []text
  ttl = 24h
}
```

Codegen emits a typed store with three methods:

```go
store := &consumer.EphemeralOutfitStore{MC: memcachedClient}

// Write with the declared TTL.
store.Set(ctx, "outfit:abc123", consumer.EphemeralOutfitValue{
    VariantIds: []string{"v1", "v2"},
    ProductIds: []string{"p1", "p2"},
})

// Read by key.
val, err := store.Get(ctx, "outfit:abc123")

// Explicit removal (optional — TTL handles cleanup).
store.Delete(ctx, "outfit:abc123")
```

## When to use ephemeral vs entity

| | `entity` | `ephemeral` |
|---|---|---|
| Storage | Postgres row | Memcached entry |
| Durability | Survives restarts, replicated | Lost on cache eviction or restart |
| TTL | `ttl_field` + SweepExpired job (Postgres DELETE) | Native memcached TTL (zero I/O on expiry) |
| Queryability | Full SQL (indexes, FKs, JOINs) | Key-lookup only |
| Use case | Durable state, relationships, audit | Short-lived scratch data the caller can regenerate |

Use `ephemeral` when:
- The data is generated per request or per session and has a natural expiry (hours, not days).
- Loss is acceptable — the caller can regenerate or the client can re-supply the data.
- High write + expire volume would bloat a Postgres table with dead tuples.

Use `entity` with `ttl_field` when:
- The data must survive memcached eviction or a pod restart.
- You need to query it (e.g., "find all sessions expiring in the next hour").
- Audit or compliance requires the row to exist durably until explicit deletion.

## How it works under the hood

1. **Set** serializes the value struct as JSON and writes to memcached with the declared TTL as the expiry.
2. **Get** reads the raw bytes, deserializes into the typed struct. A cache miss returns an error; the caller regenerates or falls back to a durable source.
3. **Delete** is a memcached DELETE. Optional since the TTL handles cleanup.
4. **Key namespacing**: the generated code prefixes every key with `eph:<namespace>.<Name>:` so ephemeral entries don't collide with entity cache entries or each other.

The memcached client is the same one atlantis already runs for entity read-through caching. No new infrastructure.

## Limits

- **Value size**: memcached's default slab limit is 1 MB per key. The JSON-serialized value must fit. For most typed structs (string arrays, small payloads) this is not a concern; if you're storing large blobs, use an entity with a `bytea` column instead.
- **Eviction pressure**: memcached evicts LRU entries when memory is full, even before TTL expires. Design callers to handle a miss gracefully — either regenerate or fall back to a durable source. Ephemeral data is a performance optimization, not a durability guarantee.

## Related

- [Caching and invalidation](caching-and-invalidation.md). The entity-level cache layer ephemeral builds on.
- [Row-level TTL](../guides/row-ttl.md). The Postgres-durable alternative for data that must survive restarts.
- [Jobs and workflows](jobs-and-workflows.md). The SweepExpired job that cleans TTL-expired entity rows.
