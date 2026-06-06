# Migration ownership

Atlantis's `migrations/` directory is split by **who writes the files** and **when they run**.

## Layout

```
migrations/
├── infra/
│   ├── 0000_outbox.up.sql
│   ├── 0000_outbox.down.sql
│   └── ...
└── tidectl/
    ├── 0000_initial.up.sql
    ├── 0000_initial.down.sql
    ├── 0001_<description>.up.sql
    ├── 0001_<description>.down.sql
    └── _staged/
        ├── NNNN_tidectl_staged.up.sql
        └── NNNN_tidectl_staged.down.sql
```

`migrations/infra/` is hand-written. It holds schema for atlantis's own runtime machinery: the cache-invalidation outbox, the IR checkpoint table, the migration history tables, and anything else that isn't a caller-defined entity. These files are part of the atlantis source tree and ship with the binary.

`migrations/tidectl/` only carries content in the **regulated flow** (`ATL_ALLOW_APPLY_MUTATION=false`). In the default flow the server applies entity migrations directly via `tide apply` — no files are written here, and the directory is empty in steady state.

## Two flows for entity schema

The same set of safety mechanisms — separate history tables, advisory lock, content-hash CAS on the IR checkpoint, reversibility CI — apply in both flows. The difference is whether a human reviews the literal SQL before it runs.

### Default flow (`ATL_ALLOW_APPLY_MUTATION=true`)

This is the standard production flow.

1. A developer edits an `.atl` file in their caller repo and opens a PR.
2. Caller CI runs `tide plan --against=<prod>` — read-only validation against the live IR.
3. PR merges; caller CI runs `tide apply --against=<prod>`.
4. The server validates, acquires the advisory lock, runs the DDL, writes the new `atlantis.ir_checkpoint` row (under content-hash CAS), and inserts an audit row into `atlantis.schema_versions` — all in one Postgres transaction.
5. `NOTIFY atl_schema_changed` fires from a Postgres trigger on `ir_checkpoint`; the server's listener rebuilds entity metadata and swaps it atomically.

Nothing is written to `migrations/tidectl/` on disk; `atlantis.schema_versions` is the durable audit trail. The `atlantis_schema_migrations_tidectl` history table — `golang-migrate`'s bookkeeping for file-based migrations — stays empty in this flow because `tidectl migrate-up` is never run against the tidectl directory.

### Regulated flow (`ATL_ALLOW_APPLY_MUTATION=false`)

Choose this only when a regulator (SOX, HIPAA, PCI) requires literal SQL review before any production database change. The default flow already provides cross-caller safety, advisory locking, transactional IR writes, and a per-apply audit row in `atlantis.schema_versions`.

1. Developer opens the same caller-repo PR; CI runs `tide plan --against=<prod>`.
2. PR merges. The server refuses any `tide apply` mutation.
3. An operator (or webhook) opens a PR against the atlantis deployment repo bumping the caller's ref in `atlantis.workspace.yaml`.
4. Deployment-repo CI runs `tidectl plan`, which clones each caller at its pinned ref, runs codegen against the unioned IR, and writes `migrations/tidectl/_staged/NNNN_tidectl_staged.up.sql` + `.down.sql`.
5. The operator reviews the staged SQL. `tidectl approve` renames it into `migrations/tidectl/` at the next sequential number.
6. Deployment-repo CI is expected to re-run `tidectl plan` and diff the working tree to catch any hand edit to a promoted file. (atlantis upstream's own `codegen-check` target guards generated client code, not promoted migrations — the staged-file integrity guard belongs in your deployment-repo CI.)
7. PR merges; the deploy pipeline runs `tidectl migrate-up --migrations-dir migrations/tidectl` (writes `atlantis_schema_migrations_tidectl`) before rolling the new server image.

## Separate history tables

Each subdirectory maintains its own migration history:

| Dir | History table |
|---|---|
| `infra/` | `atlantis_schema_migrations_infra` |
| `tidectl/` | `atlantis_schema_migrations_tidectl` |

The histories are split by ownership, not by reference-freedom. Caller schema may reference infra tables — entity triggers write to the outbox, for example. Splitting the histories means `tidectl`'s sequence can grow independently of `infra`'s without breaking either tool's monotonic-version assumption. `atlantis_schema_migrations_tidectl` is only written in the regulated flow by `tidectl migrate-up`; the default flow's audit trail is `atlantis.schema_versions`, populated server-side by every `tide apply`.

## Why the split

A single migration directory shared between hand-written and emitted files creates two failure modes:

- Codegen renumbers an emitted migration after a hand-written one is added, breaking the migrate tool's monotonic-version assumption.
- A hand edit to a codegen-emitted file is silently reverted on the next codegen run unless CI diffs the working tree against a fresh codegen.

The ownership split removes both. The flow split (default vs regulated) keeps the codegen-emitted files off disk entirely when no human is going to read them, which removes the second failure mode by removing the files.

## Apply order

Infra migrations are a dependency of tidectl migrations — the outbox table must exist before any entity enqueues an invalidation, the IR checkpoint table must exist before `tide apply` can write to it — so infra runs first, always.

At deploy time:

```sh
./tidectl migrate-up --migrations-dir migrations/infra
```

In the **default flow**, that's the only on-disk migration step. Entity migrations land via `tide apply` from caller CI after the server is up.

In the **regulated flow**, follow it with:

```sh
./tidectl migrate-up --migrations-dir migrations/tidectl
```

## Adding migrations

**A new infra migration**: create the `NNNN_<name>.up.sql` and `.down.sql` pair under `migrations/infra/` by hand. Pick `NNNN` as the next sequential number above the existing files. Both files must be present; CI runs the up + down pair to catch missing or non-reversible down files.

**A new entity migration in the default flow**: there's nothing to add on disk. The developer edits an `.atl` in their caller repo and `tide apply` does the rest.

**A new entity migration in the regulated flow**: the developer edits an `.atl` in their caller repo, the operator runs `tidectl plan` (which writes the staged pair), and `tidectl approve` promotes it to the next sequential number under `migrations/tidectl/`.

## Down migrations

Every migration in both directories ships with a `.down.sql`. The CI's reversibility check runs every up + down + up triple to catch downs that drift out of sync.

`tide rollback --to=<version>` is the default-flow rollback path: the server replays the reverse migration server-side without ever materialising files. `tidectl rollback` is the regulated-flow equivalent and runs `tidectl migrate-down` against `migrations/tidectl/` files on disk.

Either way, a migration that drops data or changes column types is not safely reversible at the data level. Restore from a Postgres PITR snapshot in that case; the down migration only reverts schema.
