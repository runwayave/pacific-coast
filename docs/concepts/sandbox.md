# The sandbox

The sandbox boots an isolated, disposable copy of the production schema. State lives in process memory; closing the sandbox destroys it. The in-memory backend is a pure-Go simulator; the Postgres backend runs a real Postgres child process. The backend is picked at boot.

## Two backends

| | In-memory | Postgres |
|---|---|---|
| Implementation | Pure-Go simulator | Real Postgres child process (`fergusstrange/embedded-postgres`) |
| Boot | Sub-millisecond | 4–8 s on Linux; 8–12 s on macOS |
| SQL coverage | The subset the simulator's executor models | Full SQL |
| State ops (checkpoint, restore, fork, seed, snapshot, inspect) | Native | Not supported |
| Determinism (fixed clock) | Honored | Silently ignored — `now()` always returns wall-time |
| Footprint | Go heap | Per-instance Postgres data directory + the extracted PG binaries (~150 MB combined, version-dependent) |

Pick **in-memory** when the work is fast-iteration shaped: agent loops, "try N then rewind," schema exploration, anywhere checkpoint and fork primitives matter. Pick **Postgres** when SQL fidelity matters: custom queries, triggers, plpgsql, multi-table joins, complex CHECK constraints. The Postgres backend exposes the SQL surface and nothing else.

## The state model (sim-only)

The simulator uses copy-on-write row maps. A capture records pointers to every table's current row map and marks them shared. A subsequent write clone-on-writes the affected map, leaving the captured pointers untouched. This is why the budget for checkpoint, restore, and fork is `O(num_tables)`, not `O(num_rows)`. The row data is never copied at capture time.

The model has four primitives:

- **Checkpoint** — save the current state.
- **Restore** — rewind to a saved state. The intermediate writes are dropped.
- **Fork** — clone the current state into N independent sandboxes, each with its own checkpoint history.
- **Diff** — compute added, removed, and modified row counts per table between two checkpoints.

The Postgres backend has no equivalent and rejects calls against these primitives with a sandbox-specific error.

## Supported SQL

The simulator's SQL grammar is supplied by `pg_query_go`, the real Postgres parser packaged for Go via cgo. Every PG syntactic construct parses. What the simulator doesn't run is everything the in-memory executor doesn't yet model: multi-table joins, CTEs, GROUP BY, HAVING, UNION/INTERSECT/EXCEPT, window functions other than `COUNT(*) OVER ()`, locking clauses, NOT predicates, table aliases, table-qualified column references, and most function calls beyond `now()` and `COALESCE`.

The supported surface covers what generated code emits and what typical operator workflows need: bare-column `SELECT`, `SELECT *` (expanded against the catalog), single-table queries with `WHERE` / `ORDER BY` / `LIMIT` / `OFFSET`, INSERT / UPDATE / DELETE with `RETURNING`, `ON CONFLICT` DO NOTHING / DO UPDATE, `EXCLUDED.col` references, JSON arrow extraction (`->`, `->>`) in both projection and WHERE position, vector distance operators (`<=>`, `<->`, `<#>`), `= ANY($N)`, and integer-literal or `$N` placeholder LIMIT/OFFSET.

See [the SQL coverage reference](../reference/sandbox-sql.md) for the full grammar.

## Surfaces

The sandbox is reached two ways. The `/sandbox` page in the [console](../guides/use-the-sandbox.md) is the operator surface: boot, capture, diff, restore, all via the UI. The HTTP control plane at `/api/sandbox/*` is the programmatic surface. Agent loops and CI fixtures call it directly. Both share the same runtime; the console BFF is a thin auth and ownership layer in front of it.

Per-user sandbox count and idle TTL are governed by `SANDBOX_PER_USER_LIMIT` and `SANDBOX_TTL` — see [configuration](../reference/configuration.md). Sandboxes are ephemeral: a console restart loses them, and the TTL janitor evicts idle ones after 30 minutes by default.

## Related

- [Use the sandbox](../guides/use-the-sandbox.md) — walkthrough of the console flow.
- [Sandbox SQL coverage](../reference/sandbox-sql.md) — what the simulator's executor runs.
- [Sandbox HTTP API](../reference/sandbox-api.md) — programmatic surface.
- [Custom queries and procedures](custom-queries-and-procedures.md) — when generated `Query` isn't enough; useful for testing custom SQL in the Postgres backend.
