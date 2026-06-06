# Custom queries and procedures

`query` and `procedure` are `.atl` blocks for SQL the typed CRUD surface cannot express, such as aggregations, multi-entity joins, and multi-step transactions. Both compile to typed gRPC methods alongside the generated `Query`, `Create`, `Update`, and `Delete`.

## Custom queries

```
query OrdersForCustomer for Order {
  input  { customer_id: varchar(8), limit: int }
  output as Order
  sql touches(Order) {
    SELECT * FROM shop.order
    WHERE customer_id = $customer_id
    ORDER BY created_at DESC
    LIMIT $limit
  }
}
```

`OrdersForCustomer` becomes a typed method on the `Order` client. `for Order` ties it to the owning entity; `input` declares typed parameters; `output as Order` says rows scan into the entity's proto (the SQL's columns must match the entity's projection). For partial or computed shapes, declare an explicit `output { ... }` block — see [the DSL grammar](../reference/dsl-grammar.md).

## Procedures

```
procedure PlaceOrderAndDecrementInventory for Order {
  input { order_id: bigint, variant_id: varchar(8), quantity: int }
  steps {
    sql touches(Order) {
      INSERT INTO shop.order (id, status) VALUES ($order_id, 'pending')
    }
    sql touches(Inventory) {
      UPDATE shop.inventory
      SET on_hand = on_hand - $quantity
      WHERE variant_id = $variant_id
    }
  }
}
```

A procedure runs its steps inside a single Postgres transaction. The generated RPC commits every step or none. Inputs are shared across steps — `$variant_id` and `$quantity` are visible to step 2 just as they are to step 1.

## The `touches(...)` directive

Every `sql` block declares the entities it reads or writes with `touches(...)`. Queries register it as the read set for cache invalidation; procedure steps register it as the write set that fires invalidations after commit.

Atlantis validates parameter references and `touches(...)` targets at apply time; pure SQL errors surface when the migration runs.

## When to reach for these

- Reads that need aggregations, window functions, `DISTINCT ON`, conditional aggregates, or any shape the typed predicate language doesn't cover: `query`.
- Writes that touch more than one entity atomically, or upserts beyond `ON CONFLICT DO NOTHING` and a single-column `DO UPDATE SET`: `procedure`.

## Testing custom SQL before `tide apply`

Paste a `query` or `procedure` body into the SQL tab of the [sandbox](sandbox.md) to verify it against seed data before applying. The sandbox's in-memory backend runs the same SQL surface the in-memory executor models; for shapes outside that surface, switch the sandbox to the Postgres backend at boot time.

## Related

- [The typed query surface](the-typed-query-surface.md) — the generated `Query`/`Create`/`Update`/`Delete` methods.
- [Caching and invalidation](caching-and-invalidation.md) — how `touches(...)` keeps the cache consistent.
- [The sandbox](sandbox.md) — preview custom SQL against synthetic rows before applying.
- [The DSL grammar](../reference/dsl-grammar.md) — the full `query` and `procedure` syntax.
