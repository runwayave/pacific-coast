# Getting started

By the end of this page you'll have a running Atlantis server and a `Note` entity applied to Postgres via `tide apply`.

## Prerequisites

- Docker (or a compatible container runtime — colima, OrbStack)
- Go 1.24 or later
- `git`

## 1. Run the server

Clone the repo, install the CLI, and bring up the bundled stack:

```
git clone https://github.com/rachitkumar205/atlantis
cd atlantis
cp .env.example .env
go install ./cmd/tide
make dev-isolated
```

`make dev-isolated` starts Postgres, memcached, and Atlantis in containers backed by a throwaway volume. Watch for `gRPC server listening on :9090` in the output, then leave this terminal running and open a new one.

(A published server image will replace the `git clone` step in a future release.)

## 2. Create a workspace

Create a fresh directory for your service:

```
mkdir my-service && cd my-service
go mod init example.com/my-service
```

Add `tide.yaml`:

```yaml
caller: my-service        # identifies this service to the server
endpoint: localhost:9090  # where the server is reachable
schema_paths:             # directories scanned recursively for *.atl
  - .
```

## 3. Declare an entity

Create `schema.atl`:

```
entity Note in app {
  id         bigint primary serial
  title      varchar(200) not null
  body       text
  created_at timestamptz not null default now()
}
```

`app` is the schema namespace.

## 4. Apply the schema

```
tide apply
```

On success, `tide` prints a line like:

```
tide: ✓ applied at 2026-05-21T15:30:00Z
```

The migration has run against the local Postgres. Verify with `psql`; there is now an `app_note` table.

## What's next

- `tide list` — print every entity currently in the merged schema.
- `tide show Note` — print the canonical `.atl` text for the `Note` entity.
- [Your first custom query](your-first-custom-query.md) — declare a read that doesn't fit primary-key lookup.
- [Use the sandbox](../guides/use-the-sandbox.md) — boot a disposable copy of the schema with seed data; preview queries before they hit production.
- [Concepts](../concepts/) — the model behind `.atl`, the cache, the CLI split.

Regenerate the typed Go client after a schema change with `tide generate` — it fetches the canonical IR from the server, scopes it to the namespaces declared in `tide.yaml`'s `generate:` field, and writes proto + typed wrappers into your caller module.

Stop the stack with `Ctrl-C` in the server terminal (the volume is discarded).
