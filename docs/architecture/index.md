# Architecture overview

A map of the Atlantis system. Each section links to a deeper page.

Atlantis has two binaries: a server and two CLIs that talk to it.

**The server** is a Go binary serving gRPC. It registers an `AdminService` (schema management — `PlanSchema`, `ApplyMigration`, `GetMergedSchema`) used by `tide` and `tidectl`, plus one generated service per declared entity (`<Entity>Service` with `Get`, `Query`, `Create`, `Update`, `Delete`) used by the caller's application code at runtime. It also registers the standard gRPC Health Checking service and reflection. One process per Postgres database.

**`tide`** is the caller-side CLI. It runs in a caller's service repo, opens a gRPC connection to the server, and submits `.atl` files via `AdminService.PlanSchema` / `ApplyMigration`. It writes the server's response (SDK bytes) to local disk.

**`tidectl`** is the operator-side CLI. It runs on the server host, invokes `internal/codegen/` in-process against local `.atl` files (from `--schema-dir` or a workspace manifest), writes emitted files into the server source tree, and shells out to `migrate` for database migrations. It speaks no gRPC.

The two CLIs do not call each other. They both reuse `internal/codegen/` — `tide` reaches it through the server's RPC; `tidectl` runs it directly.

## What happens on `tide apply`

The caller's `tide` reads `.atl` files under `schema_paths` and bundles them into a `PlanSchema` gRPC request. The server lexes, parses, and validates the bundle; cross-caller breaking changes are detected here. The IR runs through the codegen, which produces a SQL migration, proto files, Go server stubs, and the typed client SDK.

Atomicity: the server runs the SQL migration plus a write to its own catalog tables (recording the new schema version and the caller responsible) inside one Postgres transaction. Codegen output written to disk is **not** in that transaction — it's emitted after a successful commit. A crash between commit and emission leaves the schema migrated but the generated files stale; the next `tide apply` or `tidectl codegen` recovers.

The generated SDK ships back to the caller in the `ApplyMigration` response, so callers do not run their own codegen toolchain. `tide` writes those bytes into `output_dir/`.

See: [codegen pipeline](codegen-pipeline.md), [schema flow](schema-flow.md).

## What happens on `Get<Entity>`

The caller invokes the generated client method against the entity's per-entity service. The server's handler checks the body cache; on a hit, it returns the cached body without touching Postgres. On a miss, it queries Postgres, stores the row in the cache with the entity's declared TTL, and returns it.

The cache key shape and the invalidation invariants are in [cache architecture](cache-architecture.md).

## What happens on `Create` / `Update` / `Delete<Entity>`

The three mutation RPCs share a single handler shape; the codegen emits parallel methods that differ only in the SQL statement they run. Inside one Postgres transaction, the data change commits and an invalidation row is inserted into the `outbox` table for each affected cache key. The outbox row ensures the invalidation survives a server crash between commit and the memcached write.

After commit, the outbox worker drains the queue and applies invalidations to memcached.

See: [cache architecture](cache-architecture.md), [migration ownership](migration-ownership.md).

## Where the source lives

| Component | Directory |
|---|---|
| Server entry point | `cmd/server/` |
| Caller CLI | `cmd/tide/` |
| Admin CLI | `cmd/tidectl/` |
| DSL lexer, parser, IR | `internal/dsl/` |
| Codegen (proto, Go, SQL) | `internal/codegen/` |
| Postgres connection, transactions, outbox writer | `internal/storage/pg/` |
| Cache client and outbox drainer | `internal/cache/` |
| Runtime helpers linked into generated code | `internal/runtime/` |
| In-process sandbox runtime (sim + embedded backends) | `internal/runtime/sandbox/` |
| Console BFF (incl. `/api/sandbox/*` layer) | `internal/console/` |
| Admin service handlers | `internal/server/admin/` |
| Generated SDK module | `clients/go/` |
| Hand-written infra migrations | `migrations/infra/` |
| Codegen-emitted migrations | `migrations/tidectl/` |

## See also

- [Codegen pipeline](codegen-pipeline.md) — `.atl` → IR → proto, Go, SQL.
- [Cache architecture](cache-architecture.md) — body cache, query-result cache, outbox worker, version pointers.
- [Schema flow](schema-flow.md) — dev (`tide apply` mirror) vs prod (workspace manifest).
- [Migration ownership](migration-ownership.md) — `infra/` vs `tidectl/` split.
- [The sandbox](../concepts/sandbox.md) — in-process disposable runtime hosted by the console BFF.
