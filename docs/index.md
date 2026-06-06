# Documentation

These docs cover installing atlantis, declaring schemas in `.atl` files, and using the generated Go client. They assume you're writing a Go service against PostgreSQL.

Start with [Getting started](getting-started/) for installation and a first query.

- [Getting started](getting-started/). Install, declare an entity, run a query.
- [Concepts](concepts/). `.atl` files, custom queries, caching, the sandbox, the `tide` / `tidectl` split.
- [How-to guides](guides/). Adding entities, writing custom queries, using the sandbox, deploying.
- [Reference](reference/). DSL grammar, type mapping, CLI flags, env vars, sandbox SQL + HTTP API.
- [Architecture](architecture/). The codegen pipeline, the cache, and the schema flow.
