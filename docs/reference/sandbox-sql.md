# Sandbox SQL coverage

Two things happen when a SQL statement reaches the in-memory sandbox: it is parsed, then executed. The two stages have different acceptance criteria.

- Parsing is delegated to `pg_query_go`, the actual Postgres parser packaged for Go. Every PG syntactic construct parses, including features the in-memory executor doesn't model. Syntax errors come back from PG with PG's own error messages.
- Execution is the in-memory executor's responsibility. A parsed statement whose shape the executor doesn't model returns `sandbox sql: unsupported: <feature>`. The error wraps a single sentinel so callers can branch on `errors.Is(err, sql.ErrUnsupported)`.

For the embedded Postgres backend, this page does not apply — that backend runs full PG. See [the sandbox concept page](../concepts/sandbox.md) for the split.

## Accepted statements

The executor accepts four top-level statement kinds: `SELECT`, `INSERT`, `UPDATE`, `DELETE`. Input is one statement per call; multi-statement input (`SELECT 1; SELECT 2`) is rejected.

### SELECT

```text
SELECT proj [, proj ...]
  [ FROM "schema"."table" ]
  [ WHERE pred ]
  [ ORDER BY col [ASC|DESC] [NULLS FIRST|LAST] [, ...] ]
  [ LIMIT { $N | int_literal } ]
  [ OFFSET { $N | int_literal } ]
```

A projection is one of:

- A bare column reference: `"col"`.
- `*` — expanded against the catalog into one bare-column projection per declared column, in declared order. `t.*` (qualified star) is accepted and treated the same.
- An expression with a required alias: `expr AS alias`. The supported expressions are vector distance (`col <=> $N::vector`) and JSON extract (`col -> 'k' -> 'k' ->> 'k'`).
- The window-total form `COUNT(*) OVER () AS alias`. Only this one window shape is supported; `PARTITION BY` and `ORDER BY` inside `OVER` are rejected.

A `FROM` clause is optional. `SELECT 1` and `SELECT now()` (no `FROM`) evaluate the projection once and return one row.

### INSERT

```text
INSERT INTO "schema"."table" (col [, col ...])
  VALUES (expr [, expr ...])
  [ ON CONFLICT (col [, col ...])
      ( DO NOTHING | DO UPDATE SET col = expr [, ...] ) ]
  [ RETURNING col [, col ...] ]
```

`ON CONFLICT ON CONSTRAINT name` is rejected (use the inline column list). Multi-row INSERT (`VALUES (a), (b)`) is rejected — codegen-emitted writes never produce it.

The `ON CONFLICT DO UPDATE SET` clause may reference the proposed row's columns via `EXCLUDED.col`. `EXCLUDED` is detected case-insensitively.

### UPDATE

```text
UPDATE "schema"."table"
  SET col = expr [, col = expr ...]
  [ WHERE pred ]
```

`UPDATE ... FROM` and `UPDATE ... RETURNING` are not supported.

### DELETE

```text
DELETE FROM "schema"."table"
  [ WHERE pred ]
```

`DELETE ... USING` and `DELETE ... RETURNING` are not supported.

## Predicates

`pred` covers single-column comparison, null tests, ANY-membership, JSON-extract comparison, and grouped boolean expressions:

| Shape | Notes |
|---|---|
| `col = expr` | Also `<`, `<=`, `>`, `>=`, `<>`, `!=`. |
| `col IS [NOT] NULL` | |
| `col = ANY ($N)` | RHS must be `$N`; the bound value is a Go slice (`[]int64`, `[]string`, etc.). |
| `col -> 'k' ->> 'k' = expr` | JSON extract chain; the final operator decides whether the comparison runs against text or subtree. |
| `( pred AND pred ... )` | Top-level `AND` chains flatten into a flat list of predicates. |
| `( pred OR pred ... )` | Wrapped in an OR group. |

`NOT (pred)` is not supported.

## Expressions

Expressions appear in INSERT values, UPDATE SET, the right side of comparisons, projections-with-alias, and ORDER BY:

| Shape | Notes |
|---|---|
| `$N` | Placeholder. Position is 1-indexed. |
| `'string'` | String literal. |
| `123` / `-123` | Integer literal. Other literal kinds (boolean, NULL, float) are not supported. |
| `now()` | The clock. Returns the simulator's configured clock when `Deterministic` is on, otherwise wall-time. |
| `COALESCE($N::type, default)` | Two-argument only. The type cast on the first argument is parsed but discarded; the executor relies on Go-side typing. |
| `EXCLUDED.col` | Only inside `ON CONFLICT DO UPDATE SET`. |
| `col -> 'k' ->> 'k'` | JSON extract chain, in projection or ORDER BY position. The final operator determines whether the result is text or a subtree. |
| `col <=> $N::vector` | Vector distance. Operators: `<=>` cosine, `<->` L2, `<#>` inner product. PG 17's `<+>` hamming is parsed but rejected. |
| `$N::type` | Type cast. Discarded — Go-side bind values are already typed. |

## What is not supported

Every shape below parses but execution rejects with `sandbox sql: unsupported: <feature>`. The error shape is uniform, so callers can use it for differential testing against the Postgres backend.

FROM clause:

- `JOIN` of any kind. Single-table FROM only.
- Multi-table `FROM "a", "b"`.
- Subqueries in FROM (`FROM (SELECT ...) sub`).
- Table aliases (`FROM "t" AS "x"`).
- Table-qualified column references (`"t"."col"` — only the trailing `col` is honored).

Clauses:

- `WITH` / `WITH RECURSIVE` (CTEs).
- `GROUP BY`, `HAVING`.
- `UNION`, `INTERSECT`, `EXCEPT`.
- `WINDOW` clauses other than `COUNT(*) OVER ()`.
- `FETCH ... WITH TIES`.
- `FOR UPDATE`, `FOR SHARE`, locking clauses generally.

Statement modifiers:

- `RETURNING` on UPDATE or DELETE.
- `USING` on DELETE; `FROM` on UPDATE.
- ON CONFLICT `ON CONSTRAINT` (use the column list form).
- Multi-row INSERT VALUES.

Expressions and predicates:

- `NOT` predicates.
- Function calls other than the recognised special-cases (`COALESCE`, `now()`).
- Boolean literals (`TRUE` / `FALSE`).
- Bare `NULL` literals.

## Error wrapping

Every rejection wraps a single sentinel:

```go
package sql
var ErrUnsupported = errors.New("sandbox sql: unsupported")
```

Calling code uses `errors.Is(err, sql.ErrUnsupported)` to distinguish "this is not in the supported surface yet" from a syntax error or a runtime error (catalog miss, type mismatch). Syntax errors from `pg_query_go` are also wrapped with `ErrUnsupported` so the branching surface stays simple.

## Behavioural pins

- **Empty input** (`""`, whitespace-only, comment-only) — `ErrUnsupported: empty SQL`.
- **Multi-statement input** — `ErrUnsupported: multiple statements`.
- **Trailing semicolons** — accepted, treated as the statement terminator.
- **Leading or interleaved comments** (`--`, `/* */`) — parsed and skipped.
- **Quoted identifiers** — case preserved. `"Foo"` is distinct from `"foo"`.
- **Unquoted identifiers** — lowercased by the parser, matching PG behaviour. `Foo` becomes `foo`.
- **Schema qualification** — the catalog keys tables by `schema.name`. Entities declared with a `table "schema.name"` override use the override; otherwise the catalog key is `atlantis.<namespace>_<entity>` (codegen's canonical form).

## Related

- [The sandbox](../concepts/sandbox.md) — backend split, state model.
- [Sandbox HTTP API](../reference/sandbox-api.md) — programmatic surface.
- [Use the sandbox](../guides/use-the-sandbox.md) — console walkthrough.
- [The typed query surface](../concepts/the-typed-query-surface.md) — generated code's predicate tree, separate from the sandbox SQL surface.
