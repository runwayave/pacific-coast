# `tide` CLI

```
tide <command> [flags]
```

The caller-side CLI. Run from a service repo containing one or more `.atl` files and a `tide.yaml`.

`-h` and `--help` are accepted by `tide` and every subcommand.

## Configuration file

`tide` reads `./tide.yaml` from the current directory (overridable per command with `--config <path>`). The file must contain at minimum:

```yaml
caller: <name>                # required
endpoint: <host:port>         # required
schema_paths:                 # required — at least one directory
  - internal/foo
  - internal/bar
output_dir: internal/gen/pcclient   # required for `tide generate`
generate:                     # required for `tide generate` — namespaces to emit
  - consumer
  - vendor
tls:                          # optional
  cert: <file>
  key:  <file>
  ca:   <file>
```

`output_dir` is the directory inside the caller's own Go module where `tide generate` writes the typed client. `generate` lists the namespaces the caller consumes (its own plus any it reads cross-namespace). Both are only required for `tide generate`; the other commands ignore them.

YAML does not expand `${VAR}` placeholders. The config loader rejects literal `${VAR}` strings in the three TLS fields specifically (other fields are passed through verbatim).

### `schema_paths` semantics

Each path is walked recursively; every file with extension `.atl` is included. Order does not affect schema resolution. Paths are recorded relative to the caller's repo root so server-side error messages stay useful in the caller's context.

## Environment variables

| Variable | Overrides | Notes |
|---|---|---|
| `ATL_CALLER` | `caller` | |
| `ATL_ENDPOINT` | `endpoint` | |
| `TIDE_TLS_CERT` | `tls.cert` | Path to mTLS client certificate. |
| `TIDE_TLS_KEY` | `tls.key` | Path to mTLS client key. |
| `TIDE_TLS_CA` | `tls.ca` | Path to CA certificate for verifying the server. |
| `ATL_GENERATE` | `generate` | Comma-separated namespace list; replaces the `generate:` field for `tide generate`. |

`TIDE_CALLER` and `TIDE_ENDPOINT` are not consulted; use `ATL_CALLER` / `ATL_ENDPOINT`.

No other environment variables are read.

## Commands

Every command accepts `--config <path>` (default `tide.yaml`) and `--timeout <duration>`. Duration uses Go's `time.ParseDuration` format (e.g. `30s`, `1m`, `500ms`). Default `30s`.

### `tide apply`

Submits the local `.atl` files to the server, runs the migration, and prints a hint for the caller to regenerate the typed Go client. No endpoint override flag — `apply` always targets the configured `endpoint`.

```
tide apply [--backfill <file>] [--dry-run] [--no-pull]
```

| Flag | Description |
|---|---|
| `--backfill <file>` | Accepted for forward compatibility with backfill-required plans. The server does not yet splice the file into the migration; the caller applies the SQL manually before re-running `tide apply`. |
| `--dry-run` | Plan only; do not apply. Same exit codes as a real apply. |
| `--no-pull` | Skip the automatic `tide pull` before the apply. Use when offline or when the local cache is known-current. |

The default flow runs `tide pull` first so cross-caller references resolve against the freshest merged schema.

### `tide plan`

Validates the local schema against the server and reports what would change. Performs no server-side writes.

```
tide plan [--against <host:port>] [--format {table|json}] [--no-pull]
```

| Flag | Description |
|---|---|
| `--against <host:port>` | Override the configured endpoint for this command only. |
| `--format {table|json}` | Default `table`. `json` emits the raw planning response for downstream tools. |
| `--no-pull` | Skip the pre-plan refresh of `.tide-cache/`. |

### `tide pull`

Downloads the merged schema into `.tide-cache/schema/` and records the server's schema version in `.tide-cache/version.json`. Subsequent pulls short-circuit when the version matches.

```
tide pull [--force]
```

| Flag | Description |
|---|---|
| `--force` | Pull even if the local cache version equals the server's. |

`.tide-cache/` mirrors every caller's currently-registered `.atl` files. It is not the generated Go client. Add it to `.gitignore`.

### `tide generate`

Generates the typed Go client SDK into the caller's own repo, scoped to the namespaces in `generate:`. Run from the caller repo root.

```
tide generate
```

The flow:

1. Fetch the canonical IR from the server (the `GetCanonicalIR` admin RPC). The canonical IR is the server's persisted schema checkpoint, with proto field numbers already assigned. Pulling those numbers from the server means the generated wire format matches it exactly — the caller never re-derives numbers locally.
2. Filter the IR to the `generate:` namespaces. The caller gets typed clients only for what it consumes, not every caller's entities.
3. Read the caller's `go.mod` to compute the package prefix `<module>/<output_dir>`.
4. Emit proto sources (the scoped namespaces plus the embedded `atlantis/common/v1` protos) and the typed Go wrappers into `output_dir`, then shell out to `buf generate` for the `.pb.go` wire types and `gofmt` the result.

Requirements:

- `output_dir` and a non-empty `generate:` list in `tide.yaml`.
- [`buf`](https://buf.build/docs/installation) on `PATH`.
- Run from the caller repo root (so `go.mod` is readable).

The generated tree lives in the caller's module (e.g. `internal/gen/pcclient/{pb,client}/...`) and is imported with the caller's own import path. Commit it like any other generated code — it is the caller's source, not a shared artifact. There is no dependency on a central `atlantis-go` SDK for the generated types; only the hand-written `atlantis-go/jobs` runtime remains a normal library dependency for callers that run job workers.

Re-run `tide generate` after any `tide apply` that changes a namespace the caller consumes.

### `tide list`

Fetches the merged schema and prints the path of every `.atl` file, sorted lexically.

```
tide list
```

### `tide show <substring>`

Fetches the merged schema and prints the canonical `.atl` text of every file whose full path contains the substring. Case-sensitive match.

```
tide show <substring>
```

Exits non-zero if no file matches.

### `tide version`

Prints `pc <version>` to stdout (the literal `pc` prefix is a vestige of the previous binary name). Does not contact the server.

```
tide version
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, or no-op (e.g., `tide pull` with the local cache already current) |
| 1 | Backfill required — `tide apply` or `tide plan` returned a backfill-required class |
| 2 | Unknown subcommand passed to `tide` itself, **or** cross-caller breaking change returned by `apply`/`plan` |
| 3 | Operational error: parse/validation failure, network error, config error, or unknown plan class |

`tide apply` and `tide plan` share their code map exactly. Code 2 covers two unrelated conditions: an unknown subcommand passed to `tide`, and a cross-caller breaking change. CI scripts that need to distinguish them must parse stderr.

## Cache layout

`tide pull` writes to a local cache at `.tide-cache/`:

```
.tide-cache/
├── schema/
│   └── <namespace>/
│       └── <entity>.atl
└── version.json
```

`tide list` and `tide show` fetch from the server on every invocation; they do not read the cache. Deleting `.tide-cache/` only affects the next `tide pull` (and the automatic pre-pull inside `tide apply`/`plan`).

## Output

Diagnostic and progress messages are prefixed `tide:` on stderr and stdout.
