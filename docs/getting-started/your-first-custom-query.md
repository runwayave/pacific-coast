# Declare a custom query

How to add a custom SQL query to an entity's `.atl` file and register it with the server. Builds on [Getting started](.).

## When you need this

The typed `Query` method covers predicate-based reads on a single entity. For aggregations, window functions, `DISTINCT ON`, multi-entity joins, or any SQL the typed predicate language doesn't reach, declare a `query` block.

## 1. Add a `query` block to the schema

Append to `schema.atl`:

```
query NoteCountByMonth for Note {
  input  { since: timestamptz }
  output { month: timestamptz, count: bigint }
  sql touches(Note) {
    SELECT date_trunc('month', created_at) AS month,
           COUNT(*)                        AS count
    FROM app.note
    WHERE created_at >= $since
    GROUP BY 1
    ORDER BY 1
  }
}
```

`touches(Note)` tells the cache which entities to invalidate on writes.

## 2. Apply

```
tide apply
```

The server validates the SQL against the schema and registers the query. Confirm with:

```
tide show NoteCountByMonth
```

If the canonical text comes back, the query is live in the merged schema.

## What's next

- [Custom queries and procedures](../concepts/custom-queries-and-procedures.md) — when to reach for `query` vs `procedure` and how `touches(...)` keeps the cache consistent.
- [DSL grammar](../reference/dsl-grammar.md) — the full `query` and `procedure` syntax.
- [Use the sandbox](../guides/use-the-sandbox.md) — paste the query body into a disposable sandbox to verify the result shape before `tide apply`.

Calling `NoteCountByMonth` from a Go program needs the typed client; run `tide generate` from your caller repo to write it into the module declared by `tide.yaml`'s `output_dir`.
