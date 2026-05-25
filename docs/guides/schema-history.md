# Schema history

After this recipe you'll have explored the version history of your deployed schema, diffed two versions, identified who owns and who introduced each field, and performed a rollback.

Prereqs:

- atlantis server running (`tidectl dev` or a production deployment).
- At least one `tide apply` completed (so the registry has at least one version).
- `tide` on `$PATH`, configured via `tide.yaml`.

## 1. View history

```bash
tide history
```

Output (a graphical timeline, newest first):

```
  Schema History

  ● v7  apply  shopify     2h ago
  │      +4 change(s)
  │
  ● v6  apply  consumer    1d ago
  │      +2 change(s)
  │
  ● v5  apply  shopify     Jun 12, 14:30
  │      +16 change(s)
  │
  ● v4  apply  auth        Jun 10, 11:00
  │      +1 change(s)
  │
  ● v3  apply  consumer    Jun 09, 09:55
  │      +1 change(s)
  │
  ● v2  apply  shopify     Jun 08, 16:20
  │      +36 change(s)
  │
  ◌ v1  seed   auth        Jun 07, 10:00
         no changes
```

Each row is a schema version created by `tide apply`. The dot indicates the event type (● for apply/rollback, ◌ for seed). Use `--limit=N` to show only the most recent N versions.

```bash
tide history --limit=3
```

## 2. Compare versions

```bash
tide diff 3 7
```

Output:

```
+ entity vendor.VendorImport
    + vendor_id       varchar(7)  not null
    + import_strategy varchar(20) not null
    + status          varchar(10) not null

~ entity consumer.Order
    + shipping_label  text
    ~ total           int -> bigint

- entity consumer.LegacyCart
```

The diff is structural, not line-level. `+` means added, `-` means removed, `~` means modified. Field-level changes show old and new values for type or constraint changes. Both arguments are required.

## 3. Check ownership

```bash
tide owners
```

Output (one row per entity):

```
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

Each row shows which caller introduced the entity, the version it was introduced in, and the current field count. Useful for understanding ownership boundaries before making cross-caller schema changes.

## 4. Blame a field

```bash
tide blame consumer.Order
```

Output (six columns — introduced-by/at, last-modified-by/at, and status):

```
Blame: consumer.Order

FIELD                    INTRODUCED BY    AT       MODIFIED BY      AT       STATUS
(entity)                 consumer         1        consumer         1        active
id                       consumer         1        consumer         1        active
user_id                  consumer         1        consumer         1        active
item_ids                 consumer         1        consumer         1        active
total                    consumer         3        consumer         3        active
shipping_label           shopify          5        shopify          5        active
```

Each row shows which caller introduced the field, who last modified it, and whether it is still active. The `shipping_label` field was added by the `shopify` caller in version 5, not `consumer` — this is the kind of cross-caller attribution that `git blame` on the `.atl` file alone cannot reveal, since the server merges submissions from multiple callers.

## 5. Rollback

Preview what a rollback would do:

```bash
tide rollback --to=5 --dry-run
```

Output:

```
Rollback preview: v7 -> v5

3 change(s) would be applied.
(use without --dry-run to execute)
```

When satisfied, run without `--dry-run`:

```bash
tide rollback --to=5
```

The rollback creates a new version (version 8 in this case) whose IR snapshot matches version 5. The versions between 5 and 7 remain in history — nothing is deleted.

## Common errors

- **`schema version N not found`** — the version number exceeds the latest version in the registry, or the registry is empty. Run `tide history` to see available versions.
- **`rollback would break caller X: entity Y is referenced`** — another caller submitted schema that depends on an entity or field that would be removed by the rollback. Coordinate with the owning caller to remove the dependency first, or roll back to a version that still includes the referenced entity.
- **`not connected to server`** — `tide` cannot reach the endpoint in `tide.yaml`. Check that the atlantis server is running and the endpoint is correct.

## Related

- [Schema versioning](../concepts/schema-versioning.md) — how the version registry works under the hood
- [`tide` vs `tidectl`](../concepts/tide-vs-tidectl.md) — which CLI runs where
- [Deploy to production](deploy-to-production.md) — production checklist including version management
