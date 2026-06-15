# Adopt an existing database

After this recipe you'll have atlantis running against your existing Postgres database. The same tables, the same rows.

Prereqs:

- A Postgres database whose tables you want atlantis to manage.
- atlantis built and deployable; see [Deploy to production](deploy-to-production.md).
- One or more service repos with `tide.yaml` and `.atl` files in progress (or a starting point — see [Add a new entity](add-a-new-entity.md) for the basic shape).

## When to use this

atlantis creates entity tables under an `atlantis` schema by default: `atlantis.consumer_account` for `entity Account in consumer`. For an existing database the entity tables already live elsewhere. Use the `table "<schema.table>"` entity modifier to point atlantis at them. Without it, atlantis targets its default location and the existing tables go unused.

## Two ways to baseline

There's a dedicated operator command for this — `tidectl adopt` — and a manual `plan` / `apply` loop. They share one diff engine, so they classify the same disagreements; pick by who's driving:

- **`tidectl adopt` (recommended)** introspects the live DB, diffs it against every caller's declared `.atl` in your workspace, and records the IR checkpoint **without running any DDL**. It baselines all callers atomically in one transaction, buckets every difference into addition / removal / mismatch, and **blocks on a mismatch** (a field both sides describe differently) unless you pass `--allow-drift`. Every adopt — clean or drift-accepted — is recorded in `atlantis.adopt_history`. Steps 5–6 below cover it.
- **Manual `tide plan` + `tide apply`** is the per-caller path when an operator isn't running the workspace-wide command. Steps 4–5 cover it. It does *not* write `adopt_history`, and its zero-change promise has one exception — see [Legacy unique indexes can block apply](#legacy-unique-indexes-can-block-apply).

Either way the inventory and `table "..."` declaration work (steps 1–3) is the same.

## 1. Inventory the existing schema

For each table you want atlantis to manage, capture:

- The schema name (often `public`, sometimes a custom schema like `consumer` or `vendor`).
- The table name as Postgres knows it (often plural — `accounts`, `products`).
- Every column's name and type. These must match the `.atl` field declarations exactly.
- The primary key, foreign keys (with their `ON DELETE` rules), unique constraints, and indexes.

A `pg_dump --schema-only` against a staging clone gives you the full picture in one file. Read it before writing any `.atl`.

## 2. Declare each entity with `table "..."`

In your service repo:

```atl
entity Account in consumer {
  table "consumer.accounts"

  id            varchar(8) primary
  email         varchar(255) not null unique
  password_hash varchar(255)
  is_active     boolean not null default true
  created_at    timestamptz not null default now()
  updated_at    timestamptz not null default now()
  deleted_at    timestamptz

  soft_delete by deleted_at
  touch_on_update by updated_at
}
```

The `table "..."` value follows `[schema.]table` shape — each segment matching `[A-Za-z_][A-Za-z0-9_]*`. A bare name (`table "vendors"`) without a schema prefix lands in `public`.

Two constraints:

- Field names must match the column names byte-for-byte. If prod has `is_email_verified` and your `.atl` declares `email_verified`, atlantis will issue DDL for a column that doesn't exist.
- Each `table "..."` value must be unique across all declared entities. Two entities claiming the same physical table is rejected at `tide plan`.

## 3. Apply atlantis's infra migrations

```
PG_URL=postgres://... make migrate-up-infra
```

This creates six bookkeeping tables under a new `atlantis` schema: `caller_registrations`, `ir_checkpoint`, `cache_invalidations`, `cache_invalidations_dead`, `backfill_plan`, and `backfill_field_state`. The `atlantis` schema is created if absent. Existing tables in other schemas are not touched.

(The Makefile target wraps `golang-migrate` with the `x-migrations-table` parameter that keeps the infra history separate from the codegen-emitted history. See [Migration ownership](../architecture/migration-ownership.md) for why the two are split.)

## 4. Plan against the existing database

From each caller repo:

```
tide plan
```

The expected result is **zero schema changes**. `tide plan` reports `class: additive` with no `CREATE TABLE` or `ALTER TABLE` in the emitted SQL. atlantis sees the `.atl` files, computes the DDL they imply, and compares to the existing tables; everything should already match.

If `tide plan` reports unexpected DDL, one of three things is wrong:

- A field in `.atl` doesn't match a column in prod (name or type mismatch). Fix the `.atl`.
- An entity is missing its `table "..."` modifier and atlantis is targeting `atlantis.<ns>_<entity>` instead of the prod table. Add the modifier.
- The `.atl` declares a column, constraint, or index that doesn't exist in prod. Either add it to prod via a separate migration first, or remove it from `.atl`.
- The DDL diff is empty but apply still refuses. A zero-change plan is not a guarantee that `tide apply` will succeed: a legacy bare unique index the schema doesn't declare blocks apply, and it shows up only in `tide plan --format=json` (the human table omits it). See [Legacy unique indexes can block apply](#legacy-unique-indexes-can-block-apply).

Iterate until the diff is empty. That confirms atlantis's view matches the database byte-for-byte.

## 5. Baseline the checkpoint

**With `tidectl adopt` (recommended).** From the directory holding your workspace manifest:

```
tidectl adopt
```

This introspects the live DB, diffs it against every caller's `.atl`, and records the IR checkpoint for all of them in one transaction. A clean run prints `declared schema matches live DB` and exits 0. Outstanding additions (declared but not yet in the DB) and removals (in the DB but undeclared) are reported but don't block — they're outstanding work for a later `tide apply`. A **mismatch** — a field both sides describe differently — blocks the baseline and exits 1; resolve it by editing the `.atl` to match prod, migrating prod to match the `.atl`, or re-running with `--allow-drift` to baseline anyway (the drift is recorded in `atlantis.adopt_history` for audit).

**With `tide apply` (per-caller).** When you're driving one caller at a time:

```
tide apply
```

Since the schema matches, the apply is a metadata write only: atlantis records the caller's `.atl` files in `caller_registrations` and updates `ir_checkpoint`. No DDL runs against the entity tables. Repeat for each caller repo.

### Legacy unique indexes can block apply

A zero-change plan does **not** guarantee a clean apply. atlantis enforces uniqueness it declares (`unique`, `unique by …`) as a Postgres `UNIQUE` *constraint*; a legacy database often carries the same uniqueness as a bare `CREATE UNIQUE INDEX` with no backing constraint. The DSL can express a *partial* unique index (`unique index partial`) but not a non-partial bare unique index — non-partial uniqueness is declared as a `UNIQUE` constraint (`unique` / `unique by`), not an index. So an undeclared non-partial index never appears in the DDL diff — but it's a live constraint that will silently reject writes atlantis thinks are legal.

`tidectl adopt` surfaces such an index as a removal (present in the live DB, undeclared), and `tide apply` **refuses** rather than baseline over it:

```
apply blocked: the live database enforces UNIQUE index(es) this schema does not declare.
Applying would leave a hidden constraint that silently rejects legitimate writes.

  vendor.product_variants — UNIQUE index on (sku)
    resolve: DROP INDEX "vendor"."idx_product_variants_sku_unique";
    or declare the uniqueness in your .atl (field `unique`, or `unique by sku`)
```

The error prints the **live** index name verbatim. Resolve it one of these ways:

- `DROP INDEX <name>;` against the live DB, if the index is redundant or unwanted.
- Declare the uniqueness in the `.atl` so atlantis owns it — match the live index's shape:
  - **Non-partial index** — add `unique` on the field, or `unique by a, b` on the entity. Classified backfill-required (see [Add a new entity](add-a-new-entity.md)).
  - **Partial index** (`... WHERE <pred>`) — add `unique index partial by <cols> where <pred>`. You don't need to copy the live predicate verbatim; atlantis normalizes both sides through Postgres before comparing, so casts, `IN` vs `= ANY(ARRAY[…])`, and operand order don't matter (see [DSL grammar](../reference/dsl-grammar.md#entities) for the predicate syntax). Classified additive — but the apply still fails if rows matching the predicate contain duplicates; dedupe them first.
- Set `ATLANTIS_ALLOW_INDEX_DRIFT=1` in the apply environment to proceed knowingly (prefix `ATLANTIS_`, not `ATL_`; value exactly `1`).

`tide plan` only **warns** about drift — it doesn't change the plan class or exit code, and the warning data lives only in `--format=json` (`index_drift`, `index_drift_notes`, `index_drift_error`); the human table output omits it. A partial unique index (`... WHERE <pred>`) is recognized when the schema declares a matching `unique index partial` (same columns, equivalent predicate); otherwise it counts as drift.

## 6. Bring atlantis up and cut over

Start the server pointing at the same Postgres. Issue a read through atlantis's typed client and confirm it returns rows that match what direct SQL sees.

Follow the standard cutover pattern: flag-gate your application's atlantis adapters next to the existing pgx code, then flip flags per package during a maintenance window. Both code paths read and write the same physical tables, so the flip is a routing change rather than a data change.

## Common errors

- `<entity>: invalid table name "<value>": must match [schema.]table where each part is [A-Za-z_][A-Za-z0-9_]*` — the value has a syntactically invalid identifier (embedded space, leading digit, multiple dots, embedded quotes). Use a simple two-part name.
- `table "<name>" is claimed by both <A> and <B> — each \`table "..."\` value must be unique` — two `.atl` entities mapped to the same physical table. Each value must be unique across the merged IR.
- `tide plan` exits with class `cross_caller_breaking` and the breaking-detail line `<entity>/: table override changed: "<old>" -> "<new>" (manual ALTER TABLE RENAME required)` — the modifier value moved relative to the last applied IR. atlantis won't auto-rename; run `ALTER TABLE "<old-schema>"."<old-table>" RENAME TO "<new-table>"` (and `ALTER TABLE ... SET SCHEMA ...` if the schema is also changing) manually before re-applying.

## Related

- [DSL grammar reference](../reference/dsl-grammar.md#entity-level-clauses) — the `table` modifier alongside other entity-body clauses.
- [Migration ownership](../architecture/migration-ownership.md) — why atlantis's bookkeeping tables stay in the `atlantis` schema even when entity tables do not.
- [Deploy to production](deploy-to-production.md) — operator-side runbook for standing up the server.
- [`tidectl` CLI reference](../reference/cli-tidectl.md) — `tidectl adopt` and the other operator commands.
