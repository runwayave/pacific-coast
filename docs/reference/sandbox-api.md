# Sandbox HTTP API

The sandbox exposes a JSON-over-HTTP control plane. There are two layers:

- **The console BFF** at `/api/sandbox/*` — authenticated, owner-scoped, per-user limit enforced. The console UI and external agents both reach the sandbox through this surface.
- **The runtime** at `/v1/sandbox/*` — the in-process control plane the BFF proxies to. Not directly exposed; documented here only because the BFF's response shape passes through unchanged.

Both layers carry server-measured latency on every response: a `t_server_us` field on JSON bodies and an `X-Atl-Server-Us` header on binary responses.

## Authentication and ownership

Every `/api/sandbox/*` route requires an authenticated session. Mutating routes additionally require the CSRF token (same flow as the rest of the console).

Each sandbox is minted a 128-bit opaque `pub_id` at boot. The runtime's internal sequential id is never exposed; the BFF maintains a `pub_id → internal_id` table keyed by owner. A request whose `pub_id` doesn't resolve to the caller's owner record returns `404` — the same response shape as "doesn't exist," so enumeration is blocked.

## Limits

| Limit | Default | Configured by |
|---|---|---|
| Concurrent sandboxes per user | 3 | `SANDBOX_PER_USER_LIMIT` |
| Idle TTL before janitor evicts | 30 min | `SANDBOX_TTL` |
| `PUT /snapshot` request body | 256 MiB | constant |

A boot beyond the per-user limit returns `429 Too Many Requests`. A snapshot body beyond 256 MiB returns `413 Payload Too Large`.

## Lifecycle endpoints

### `POST /api/sandbox`

Boots a sandbox. Request body:

```json
{
  "backend": "sim" | "embedded",
  "determinism": "strict" | null,
  "seed": 42
}
```

All fields are optional. Empty body is equivalent to `{"backend": "sim"}`. `determinism: "strict"` is silently ignored on the embedded backend.

Response `200`:

```json
{
  "pub_id": "5fa3b21c…",
  "backend": "sim",
  "boot_ms": 0,
  "schema_version": "f1f9b85…",
  "entity_count": 37
}
```

Boot is the only endpoint whose JSON response does **not** carry `t_server_us`; `boot_ms` is the timing field for this endpoint.

Errors: `429` (per-user limit hit), `502` (admin RPC for current IR failed), `400` (validation).

### `GET /api/sandbox`

Lists the caller's active sandboxes. List requests do **not** touch the activity timestamp, so polling here doesn't keep an idle sandbox alive.

Response `200`:

```json
{
  "sandboxes": [
    {
      "pub_id": "5fa3b21c…",
      "backend": "sim",
      "schema_version": "f1f9b85…",
      "created_at": "2026-06-04T18:00:00Z",
      "last_active": "2026-06-04T18:12:30Z",
      "boot_ms": 0
    }
  ]
}
```

### `DELETE /api/sandbox/{pub_id}`

Destroys a sandbox, freeing memory and (for embedded) the on-disk Postgres data directory. Response `204`.

## SQL execution

### `POST /api/sandbox/{pub_id}/sql/exec`

Runs a non-returning statement (UPDATE / DELETE / INSERT without RETURNING).

Request:

```json
{ "sql": "UPDATE \"s\".\"t\" SET \"a\" = $1 WHERE \"id\" = $2",
  "args": [1, "user_001"] }
```

Response `200`:

```json
{ "rows_affected": 1, "t_server_us": 87 }
```

### `POST /api/sandbox/{pub_id}/sql/query`

Runs a query that returns rows (SELECT, INSERT/UPDATE/DELETE with RETURNING).

Response `200`:

```json
{
  "rows": [
    { "id": 1, "email": "a@x" },
    { "id": 2, "email": "b@x" }
  ],
  "t_server_us": 143
}
```

Column names come from the row cursor's `Columns()` method, which knows the post-expansion projection. `SELECT *` is reported with the catalog's declared column names rather than a literal `*`.

`pub_id` accepts both backends. For SQL the in-memory backend covers the subset documented in [sandbox-sql.md](sandbox-sql.md); the embedded backend runs full Postgres.

## Inspect (sim-only)

Inspect endpoints are sim-only — Catalog, Describe, Sample, Find, and Diff. A call against an embedded sandbox returns `400 Bad Request` with `<feature> is in-memory only; boot a Sim sandbox to use it`.

### `GET /api/sandbox/{pub_id}/inspect/catalog`

Returns the qualified entity names the simulator's catalog has registered.

```json
{ "entities": ["consumer.accounts", "vendor.vendors", …], "t_server_us": 4 }
```

The console's Tables and Seed dropdowns populate from this.

### `GET /api/sandbox/{pub_id}/inspect/describe?q={qualified}`

Returns the column shape + row count for one entity.

```json
{
  "qualified": "consumer.accounts",
  "schema": "consumer", "name": "accounts",
  "columns": [
    { "name": "id", "kind": "string", "nullable": false },
    { "name": "email", "kind": "string", "nullable": false }
  ],
  "primary_key": ["id"],
  "row_count": 0,
  "t_server_us": 6
}
```

`404` when the qualified name isn't registered.

### `GET /api/sandbox/{pub_id}/inspect/sample?q={qualified}&n={N}`

Returns up to N rows as a `[]map[string]any` projection. Default N is 5 when the parameter is omitted; the console's Tables tab requests `n=10` explicitly.

### `POST /api/sandbox/{pub_id}/inspect/find`

Body: `{"qualified": "...", "predicates": [{"column": "...", "op": "=", "value": ...}, ...]}`. Predicates are AND-ed. Supported `op` values are `=`, `!=`, `<`, `<=`, `>`, `>=`, `is null`, `is not null`.

### `POST /api/sandbox/{pub_id}/inspect/diff`

Body: `{"before_mark_id": "1", "after_mark_id": "2"}`. Returns per-table counts:

```json
{
  "tables": {
    "consumer.accounts": { "added": 3, "removed": 1, "modified": 0 }
  },
  "t_server_us": 12
}
```

Empty `tables` (`{}`) means no differences. `404` when either mark id is unknown to this sandbox.

## Checkpoint and restore (sim-only)

### `POST /api/sandbox/{pub_id}/mark`

Captures a checkpoint. Empty body. Response `201`:

```json
{ "mark_id": "1", "t_server_us": 142 }
```

Mark ids are sequential int36 strings, scoped to the sandbox.

### `POST /api/sandbox/{pub_id}/restore`

Rewinds to a previously captured checkpoint. Body: `{"mark_id": "1"}`. Response `204` with the timing on the `X-Atl-Server-Us` header.

`404` when the mark id is unknown. The console treats this case specially: a 404 on restore is interpreted as "the mark expired" (BFF restarted) and the offending entry is pruned from the local timeline.

## Fork (sim-only)

### `POST /api/sandbox/{pub_id}/fork`

Clones the current state into N independent sandboxes. Body: `{"n": 3}`. Each child gets a fresh `pub_id`, becomes the caller's own (per-user limit applies to the total), and has its own checkpoint history.

Response `201`:

```json
{ "ids": ["abc…", "def…", "ghi…"], "t_server_us": 4 }
```

`400` when called on an embedded-backed sandbox. `429` when minting N children would push the caller above the per-user limit.

## Snapshot (sim-only)

### `GET /api/sandbox/{pub_id}/snapshot`

Returns the sandbox's portable byte blob. `Content-Type: application/octet-stream`, timing on the `X-Atl-Server-Us` header. The blob encodes a schema signature alongside the rows; restore against a mismatched schema is rejected.

### `PUT /api/sandbox/{pub_id}/snapshot`

Restores a sandbox's state from a previously-downloaded blob. The body is the raw `.gob` bytes (octet-stream). Maximum body size is **256 MiB**; overflow returns `413`.

`400` when the blob's schema signature doesn't match the sandbox's current catalog.

## Seed data (sim-only)

### `POST /api/sandbox/{pub_id}/fixtures/bulk`

Generates and inserts N synthetic rows for an entity. Body:

```json
{ "qualified": "consumer.accounts", "n": 1000, "seed": 42, "pk_start": 1 }
```

`seed` and `pk_start` are optional. Values are produced from each column's declared type — email-shaped strings, monotonic timestamps, unit-norm vectors. When the sandbox was booted with `determinism: "strict"`, the values are reproducible from `seed`.

Response `200`:

```json
{ "inserted": 1000, "t_server_us": 21834 }
```

## Status codes

| Code | Meaning |
|---|---|
| `200` | Read, single-statement write, or boot succeeded. |
| `201` | Mark captured, fork minted, or snapshot uploaded. |
| `204` | Destroy or restore succeeded with no body. |
| `400` | Validation, sim-only feature called on embedded, unsupported SQL, or schema mismatch on snapshot restore. |
| `404` | Sandbox or mark not found, or sample/describe target absent. Returned for ownership mismatches too. |
| `405` | Wrong method on a route. |
| `413` | Snapshot upload exceeded 256 MiB. |
| `429` | Per-user sandbox limit reached (boot or fork). |
| `500` | Internal error or marshal failure. |
| `502` | Admin RPC for the current IR failed during boot. |

## Related

- [The sandbox](../concepts/sandbox.md) — backend split, state model.
- [Sandbox SQL coverage](../reference/sandbox-sql.md) — what the simulator's executor runs.
- [Use the sandbox](../guides/use-the-sandbox.md) — console walkthrough.
- [Configuration](configuration.md) — `SANDBOX_PER_USER_LIMIT`, `SANDBOX_TTL`.
