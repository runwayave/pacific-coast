# Add a new entity

After this recipe you'll have a new entity declared, validated, applied, and registered in the merged schema.

Prereqs:

- A service repo with `tide.yaml` (see [Getting started](../getting-started/) if you haven't created one).
- `tide` on `$PATH`.
- The Atlantis server reachable on `tide.yaml`'s `endpoint`.

## 1. Create the schema file

Under a path listed in `tide.yaml`'s `schema_paths`:

```
internal/orders/schema.atl
```

## 2. Declare the entity

```
entity Order in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id
  total       numeric(10, 2) not null
  created_at  timestamptz not null default now()
}
```

## 3. Plan and apply

```
tide plan
tide apply
```

`tide plan` reports what would change without mutating. `tide apply` runs the migration and updates the merged schema.

To preview behaviour before applying — seed rows, run queries, capture and diff state — boot a copy of the schema in the [sandbox](use-the-sandbox.md).

Common errors:

- `references unknown entity shop.Customer`: the referenced entity hasn't been registered yet. Run `tide apply` from the repo that declares `shop.Customer` first.
- `tide plan` exits 1 (backfill required): you've added a `not null` column without a `default` to an existing entity. Add a `default` or supply backfill SQL.
- `tide plan` exits 2 (cross-caller breaking): another caller's schema depends on a field you removed or renamed. The output names the conflict.

## 4. Verify

```
tide show Order
```

Prints the canonical `.atl` text as the server now holds it.

## Common patterns

### Server-set timestamps with auto-touch

```
created_at timestamptz not null default now()
updated_at timestamptz not null default now()
touch_on_update by updated_at
```

`touch_on_update by` installs a Postgres trigger that sets `updated_at` on every `UPDATE`.

### Soft delete

```
deleted_at timestamptz
soft_delete by deleted_at
```

The generated `Delete` RPC sets `deleted_at` to `now()` instead of dropping the row. `Get` and `Query` filter `deleted_at IS NULL` automatically. To read tombstones, declare a [custom query](../concepts/custom-queries-and-procedures.md) with the inverse filter.

### Per-tenant partition

```
vendor_id varchar(8) not null references vendor.Vendor.id
partition by vendor_id
```

Every generated read RPC injects `vendor_id = <caller-partition>` from the auth context; callers cannot override via the filter. Requires the caller's auth context to carry a partition; without it, reads return empty. Configure caller partitions in your auth interceptor before declaring `partition by`.

## Related

- [DSL grammar reference](../reference/dsl-grammar.md) — every modifier and entity-level clause.
- [Type mapping](../reference/dsl-types.md) — each `.atl` type's Postgres, Go, and proto representation.
