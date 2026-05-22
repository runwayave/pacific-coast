# atlantis

atlantis generates Postgres migrations, a typed Go client, and a cached gRPC server from a single `.atl` schema file. The same declaration produces the migration SQL, the per-entity request handler, the read-through cache, and the cache-invalidation logic. These are normally hand-written and kept in sync by convention.

```
$ tide apply
tide: ✓ applied at 2026-05-21T15:30:00Z
```

## How it works

A `.atl` file declares an entity:

```
entity Order in shop {
  id          bigint primary serial
  customer_id bigint not null references shop.Customer.id
  status      varchar(20) not null default "pending"
  total       numeric(10, 2) not null
  created_at  timestamptz not null default now()

  index by customer_id
  cache { read_through ttl=5m tag="customer:{customer_id}" }
}
```

Running `tide apply` from a service repo submits the file to the atlantis server. The server generates SQL migrations, a Go SDK, proto definitions, and per-entity request handlers, then applies the migration after the planner classifies it as additive, backfill-required, or breaking. The server serves `Order` over gRPC, with a memcached read-through cache in front of every read.

Cache invalidation rides the write transaction. Every Create / Update / Delete commits the data change and an outbox row in one Postgres txn, and a worker drains the outbox to invalidate memcached. Application code never writes the invalidation directly.

Custom SQL is typed too:

```
query OrdersForCustomer for Order {
  input  { customer_id: bigint, limit: int }
  output as Order
  sql touches(Order) {
    SELECT * FROM shop.order
    WHERE customer_id = $customer_id
    ORDER BY created_at DESC
    LIMIT $limit
  }
}
```

The `touches(Order)` clause tells the cache invalidator which entities the SQL reads. Custom queries are cached and invalidated the same way generated CRUD is.

When multiple services share the database, `tide plan` cross-checks proposed migrations against every caller's registered schema. Schema changes that would break another service's reads are rejected before the migration runs.

## Compared to sqlc, Prisma, Hasura

sqlc is the closest equivalent in Go: it generates typed query methods from hand-written SQL. The migrations, the server, and any caching stay on you. Prisma covers similar ground in TypeScript without including a server; Hasura runs a server but exposes GraphQL and is administered outside the application codebase.

atlantis produces migrations, a Go SDK, a server, and the cache-invalidation logic from one source file. It runs as a single Go binary in front of Postgres; Go services call it over gRPC.

The DSL maps to Postgres directly. Types and constraints are 1:1, and pgvector (HNSW indexes) and TimescaleDB (hypertables, chunk intervals) are supported in the DSL. Generated code uses pgx types without a mapping layer.

## Install

To run the server locally with Postgres and memcached in containers:

```
git clone https://github.com/rachitkumar205/atlantis
cd atlantis
# Add at least one .atl file under testdata/schema/ (see docs/getting-started/)
make codegen       # generates gen/, clients/go/, atlantis/ from your .atl files
make dev-isolated
```

The generated tree (`gen/`, `clients/go/client/`, `clients/go/pb/`, `atlantis/<ns>/v1/`) is gitignored. `make codegen` produces it from `testdata/schema/`; with no `.atl` files there, the codegen exits with no work to do and `cmd/server` won't compile. Add an entity first.

To use atlantis from a service repo:

```
go install github.com/rachitkumar205/atlantis/cmd/tide@latest
tide apply
```

Walkthrough: [docs/getting-started/](docs/getting-started/). Production deployment: [docs/guides/deploy-to-production.md](docs/guides/deploy-to-production.md).

## Status

Supported:

- PostgreSQL 15 or later, single server
- Go SDK
- Migrations via `tidectl migrate-up`, or `AUTO_MIGRATE=true` for development
- Memcached read-through cache with outbox-driven invalidation
- pgvector with HNSW indexes
- TimescaleDB hypertables
- Custom queries and multi-step procedures
- mTLS between client and server

Not yet supported: MySQL, non-Go client SDKs, multi-region deployments.

## License

- **Server** (this repository, except `clients/go/`) — [BSL 1.1](LICENSE). Production use is permitted except offering atlantis on a hosted or embedded basis in competition with the licensor's paid versions. Converts to Apache 2.0 on 2030-05-21.
- **SDK** (`clients/go/`) — [Apache 2.0](clients/go/LICENSE). Apps that link this SDK are unrestricted; only the atlantis server itself is under BSL.

See [`clients/go/README.md`](clients/go/README.md) for how the SDK is generated and how it connects to a server.

## Naming

Not to be confused with the Terraform tool at [runatlantis/atlantis](https://github.com/runatlantis/atlantis). This project lives at [`rachitkumar205/atlantis`](https://github.com/rachitkumar205/atlantis).
