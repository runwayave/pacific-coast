# Schema versioning

Every `tide apply` creates a numbered schema version. The version stores the full IR snapshot, a structural diff from the previous version, the generated SQL, the caller identity, and a timestamp. The version registry is append-only and is the source of truth for what has been deployed and when (the `.atl` files remain the authoring authority for schema definitions).

```
$ tide history --limit=3

  Schema History

  ● v7  apply  shopify     2h ago
  │      +4 change(s)
  │
  ● v6  apply  consumer    1d ago
  │      +2 change(s)
  │
  ◌ v5  seed   shopify     Jun 12, 14:30
         no changes

  … more versions available — use --limit
```

## What a version contains

Each version is a self-contained record:

- **IR snapshot**: the full intermediate representation of the merged schema at that point in time. Any version can be materialized into a complete `.atl` file set.
- **Structural diff**: entity-level and field-level changes from the previous version — additions, removals, type changes, constraint changes.
- **Generated SQL**: the exact migration SQL that was applied.
- **Caller identity**: which caller (from `tide.yaml`) submitted the change.
- **Timestamp**: server-side wall-clock time of the apply.

## Append-only history

Versions are never deleted and never mutated. A rollback creates a new version whose IR snapshot matches the target version, and whose diff describes the reversal. Version numbers always increase.

```
$ tide history --limit=3

  Schema History

  ● v9  rollback  operator    just now
  │      +5 change(s)
  │
  ● v8  apply     shopify     3h ago
  │      +1 change(s)
  │
  ● v7  apply     shopify     Jun 14, 09:12
         +4 change(s)

  … more versions available — use --limit
```

Version 9 is a new version, not a deletion of versions 6-8. The full history of what was deployed and when is always recoverable.

## Per-entity lineage

The registry tracks which caller introduced each entity and each field. `tide blame` resolves a single entity to its per-field attribution:

```
$ tide blame consumer.Order
Blame: consumer.Order

FIELD                    INTRODUCED BY    AT       MODIFIED BY      AT       STATUS
(entity)                 consumer         1        consumer         1        active
id                       consumer         1        consumer         1        active
user_id                  consumer         1        consumer         1        active
item_ids                 consumer         1        consumer         1        active
total                    consumer         3        consumer         3        active
shipping_label           shopify          5        shopify          5        active
```

`tide owners` shows ownership at the entity level — which callers have declared entities in the merged schema:

```
$ tide owners
ENTITY                           OWNER            SINCE   FIELDS
auth.User                        auth             v1      6
auth.Session                     auth             v1      4
auth.ApiKey                      auth             v4      3
consumer.Order                   consumer         v1      5
consumer.Invoice                 consumer         v3      4
shopify.Product                  shopify          v2      8
shopify.Variant                  shopify          v2      6
shopify.Collection               shopify          v2      5
shopify.VendorImport             shopify          v5      3
```

## Registry vs `.atl` files

The `.atl` files in caller repos are the authoring surface: you edit them, review them in PRs, and `git blame` them. The version registry is the deployment record: what was applied to the server and when.

The analogy to git:

| git | atlantis |
|---|---|
| Source files | `.atl` files in caller repos |
| Commits | Schema versions |
| Remote repository | Version registry on the server |
| `git log` | `tide history` |
| `git diff` | `tide diff` |
| `git blame` | `tide blame` |

The `.atl` files can diverge temporarily from the registry (e.g., a PR is merged but `tide apply` hasn't run). The registry reflects what is actually deployed. `tide plan` shows the delta between the local `.atl` files and the registry's latest version.

## How this differs from other tools

**Atlas (Ariga)**: Atlas stores a "desired state" and computes diffs against the live database. History is implicit in the migration directory. atlantis stores explicit versioned snapshots of the full schema IR, so any historical state can be reconstructed without replaying migrations.

**Traditional migration tools (golang-migrate, Flyway)**: These store a sequence of forward/backward SQL files. The "current schema" is whatever the last migration produced — there is no snapshot of the full schema at any point. atlantis stores the complete IR at every version, so `tide diff 3 7` reconstructs the structural change without replaying migrations 4 through 7.

## Version diffs

`tide diff` compares any two versions structurally:

```
$ tide diff 5 7
+ entity vendor.VendorImport
    + vendor_id       varchar(7)  not null
    + import_strategy varchar(20) not null
    + status          varchar(10) not null

~ entity consumer.Order
    + shipping_label  text
    ~ total           int -> bigint

- entity consumer.LegacyCart
```

The diff is entity-level and field-level, not line-level. It reports additions (`+`), removals (`-`), and modifications (`~`) with the old and new values for modified fields.

## Related

- [Schema as code](schema-as-code.md) — why `.atl` files are authoritative for authoring
- [`tide` vs `tidectl`](tide-vs-tidectl.md) — which CLI runs history/diff/blame
- [Schema history](../guides/schema-history.md) — step-by-step usage of history, diff, blame, owners, and rollback
