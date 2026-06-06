# `tide` vs `tidectl`

atlantis ships two CLIs. `tide` runs from caller service repos and is the only path that mutates schema in the default production flow. `tidectl` runs on a host with direct access to the server's database and is the operator's tool for bootstrap, audit, and the regulated opt-in.

## What `tide` does

`tide` runs from your service repo. It reads the project's `tide.yaml`, collects the `.atl` schema files it points at, and submits them to the atlantis server over mTLS.

| Subcommand | Purpose |
|---|---|
| `tide plan --against=<endpoint>` | Read-only validation. Server returns the SQL it would run, the change classification, and any cross-caller breaks (the server refuses any apply that would invalidate another caller's current schema). |
| `tide apply --against=<endpoint>` | Mutates schema on the server. Three independent gates grant permission: `ATL_ALLOW_APPLY_MUTATION=true` (wildcard, the default), `ATL_MUTATION_ALLOWED_CALLERS` (per-CN allowlist), or `caller_identities.can_mutate=true` (runtime per-caller flag set via the console). The server runs the DDL, writes `atlantis.ir_checkpoint` under content-hash CAS, and inserts an audit row into `atlantis.schema_versions` — all in one Postgres transaction, under an advisory lock. |
| `tide pull` | Resyncs local `.atl` from server state. |
| `tide diff` | Local diff between working tree and the server's IR. |
| `tide history`, `tide blame`, `tide owners` | Inspect the version registry. |
| `tide rollback --to=<version>` | Reverts to a prior IR. The server orchestrates the reverse migration the same way `apply` orchestrates a forward one. Operator-only: the calling CN must be in `ATL_OPERATOR_ALLOWED_CALLERS`. |
| `tide generate` | Regenerates the typed Go client into the caller's own module. |

## What `tidectl` does

`tidectl` runs server-side. It owns codegen artifacts, file-based migration management, and operator-only admin RPCs. Caller CI never invokes `tidectl`.

| Subcommand | Purpose |
|---|---|
| `tidectl migrate-up --migrations-dir migrations/infra` | Apply atlantis's own runtime-machinery migrations at deploy time. |
| `tidectl migrate-up --migrations-dir migrations/tidectl` | Apply staged entity migrations in the regulated flow. Not run in the default flow — the server applies entity migrations itself when callers call `tide apply`. |
| `tidectl plan` | Codegen-emits staged migration files under `migrations/tidectl/_staged/`. Used in the regulated flow to materialize a migration that an operator will review and approve before it runs. |
| `tidectl approve` | Promotes `_staged/` files to the next sequential number in `migrations/tidectl/`. Regulated flow only. |
| `tidectl history`, `tidectl blame`, `tidectl owners`, `tidectl rollback` | Server-side audit and recovery. Available regardless of mutation policy because they bypass the caller-CI surface. |

## Default flow vs regulated flow

`ATL_ALLOW_APPLY_MUTATION` controls which CLI sits in the schema-change path.

**Default (`ATL_ALLOW_APPLY_MUTATION=true`, the recommended production setting):**
- Schema changes go through `tide apply` from caller CI.
- The server is the single source of truth for migration history; the on-disk `migrations/tidectl/` directory is empty in steady state.
- `tidectl` is used only for bootstrap (`migrate-up-infra` at first deploy), audit (`history`, `blame`, `owners`), and break-glass.

**Regulated (`ATL_ALLOW_APPLY_MUTATION=false`):**
- The server rejects every `tide apply` mutation.
- Schema changes require an operator to run `tidectl plan` (codegen the SQL), `tidectl approve` (promote into `migrations/tidectl/`), then `tidectl migrate-up` against the materialized files — typically inside a deploy pipeline that opens a PR against the atlantis deployment repo so the SQL is reviewed before it runs.
- Use this when a regulator requires literal SQL review before any production database change.

## Why the split

Caller CI runs on every PR across every service repo. The blast radius of a buggy or malicious `tide apply` has to be bounded server-side:

- **mTLS pins caller identity.** The connecting client cert's CN is the caller name; the server requires `req.Caller` to match the CN. A caller can only submit `.atl` files for its own namespace because it never holds anyone else's `.atl` source.
- **Cross-caller break detection.** Even within its own namespace, the server's diff classifier refuses any apply that would invalidate another caller's current schema (e.g., dropping a column another caller reads).
- **Advisory lock.** Concurrent applies serialise; the second waits, re-plans against the new ground truth, and is rejected with a structured report if its plan no longer applies.

The CLI split keeps the caller surface narrow — `tide` can submit schema changes but cannot run codegen, and `tidectl` is not deployed to caller pipelines at all.

## Related

- [Schema as code](schema-as-code.md) — why the caller CLI is the schema-mutation surface
- [Schema flow](../architecture/schema-flow.md) — how bytes move from `.atl` to a running server
- [Migration ownership](../architecture/migration-ownership.md) — what lives in `migrations/infra/` vs `migrations/tidectl/`
- [`tide` CLI reference](../reference/cli-tide.md)
- [`tidectl` CLI reference](../reference/cli-tidectl.md)
