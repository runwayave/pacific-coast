# Use the sandbox

This walkthrough boots an isolated copy of the production schema, mutates it, captures checkpoints, and diffs two states. Everything happens through the console UI; no `tide` invocation is needed.

## Prerequisites

- A running console (`docker compose up console`, or whatever your deployment uses).
- A user account with permission to reach `/sandbox`. By default any authenticated console user can boot up to three sandboxes; the limit is `SANDBOX_PER_USER_LIMIT` in [configuration](../reference/configuration.md).

## Boot a sandbox

Navigate to `/sandbox`. The left rail has two backend options: **In-memory** (pure-Go simulator, sub-millisecond boot, supports state primitives) and **Postgres** (real PG child process, a few seconds to boot, SQL only). See [the sandbox concept page](../concepts/sandbox.md) for the full split.

Click `+ In-memory` for this walkthrough. The status strip at the top of the page shows the backend, a short opaque ID, the schema hash, and the boot time. After the first operation, it also displays the live `t_server_us` for the most recent action.

The page has five tabs:

| Tab | What it does |
|---|---|
| Tables | Browse the schema; sample 10 rows of an entity. |
| Seed data | Insert N synthetic rows generated from each column's declared type. |
| SQL | Run arbitrary SQL. SELECT/RETURNING returns rows; everything else returns rows-affected. |
| Compare | Diff added / removed / modified rows per table between two checkpoints. |
| Forks | Clone the sandbox into N independent copies. |

## Capture a baseline checkpoint

In the left rail, find the **Checkpoints** section and click **+ Capture**. A row appears: `just now · captured in NNN µs`. This is the empty schema; the diff later compares against it.

## Seed some rows

There are two ways to populate data.

**Manually via SQL.** Open the SQL tab and insert against any table:

```sql
INSERT INTO "consumer"."accounts" ("id", "email")
VALUES ('user_001', 'alice@example.com');
```

**Synthetically via Seed data.** Pick an entity from the dropdown, click `+ 100 rows` / `+ 1,000` / `+ 10,000`. Values are produced from each column's declared type — email-shaped strings, monotonic timestamps, unit-norm vectors. When the sandbox was booted with **Deterministic** checked, the values are reproducible from the seed.

Capture a second checkpoint after seeding.

## Mutate further

Run an UPDATE and a DELETE so the diff has something to show:

```sql
UPDATE "consumer"."accounts"
SET "email" = 'alice@new.example.com'
WHERE "id" = 'user_001';
```

```sql
DELETE FROM "consumer"."accounts"
WHERE "id" = 'user_002';
```

Capture a third checkpoint.

## Compare

Switch to the **Compare** tab. Pick an earlier checkpoint and a later one from the two dropdowns, then click **Compare**. A table appears:

| Table | Added | Removed | Modified |
|---|---|---|---|
| `consumer.accounts` | +1 | −1 | ~1 |

The latency for the compare itself (microseconds) shows next to the button.

## Restore

To rewind to a previous state, click the restore icon next to any checkpoint row in the left rail. The sandbox state is replaced — intermediate writes are discarded. Run a `SELECT` to confirm.

Restore preserves the checkpoint timeline. The timeline supports moving back and forth between captured states.

## Fork

The **Forks** tab clones the current state into N independent sandboxes. Each fork has its own checkpoint history and its own destruction lifecycle. Enter a count, click `Fork into N`, and child sandboxes appear in the parent's tree visualisation. Click one to switch to it.

Forks are sim-only. With a Postgres-backed sandbox active, the Fork button is disabled with an inline explanation.

## Snapshot

To persist a sandbox's state to a portable byte blob, use the **Snapshot** button in the status strip. The browser downloads a `.gob` file containing the schema signature and every row. Restoring a snapshot requires the same schema signature; a mismatch is rejected.

Snapshot is sim-only.

## Destroy

The **Destroy** button on the status strip removes the sandbox and frees its memory. Sandboxes also auto-evict after the idle TTL (`SANDBOX_TTL`, default 30 minutes), so leaving a tab open without activity will silently drop them; a console restart drops all of them.

## Test a custom query before `tide apply`

A `query` or `procedure` body is just SQL. Boot a sandbox, seed enough data, paste the body into the SQL tab, and verify the result shape before submitting the migration.

The typical flow:

1. Boot an **in-memory** sandbox.
2. Use **Seed data** to populate the entities the query reads. A few hundred rows is usually enough to validate WHERE filters and ORDER BY.
3. Open the `.atl` file with the `query` block; copy the SQL body.
4. Paste it into the SQL tab. Replace `:named` parameters with positional `$N` if the in-memory backend is being used (the simulator parses positional placeholders only; named-to-positional rewriting is the validator's job at `tide apply`).
5. Click **Execute** and inspect the rows.

If the query uses constructs the simulator's executor doesn't model — joins, CTEs, GROUP BY, window functions other than `COUNT(*) OVER ()` — switch to a **Postgres** backend sandbox at boot time. The Postgres backend runs full PG, with the same DDL the migration emits.

## Deep links

Other console pages link into the sandbox:

- The schema browser's "Try in sandbox" button on an entity detail opens `/sandbox?focus=ns.Entity` — the Tables tab pre-selects that entity.
- A pending operation's "Preview in sandbox" opens `/sandbox?boot=sim` and auto-boots a fresh in-memory sandbox.

## Related

- [The sandbox](../concepts/sandbox.md) — what the two backends are and how the state model works.
- [Sandbox SQL coverage](../reference/sandbox-sql.md) — what the in-memory executor accepts.
- [Sandbox HTTP API](../reference/sandbox-api.md) — programmatic surface for agents and CI.
