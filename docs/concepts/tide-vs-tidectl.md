# `tide` vs `tidectl`

atlantis ships two CLIs: `tide` for callers and `tidectl` for operators. The split keeps the caller surface narrow — `tide` can submit schema changes but cannot run codegen or touch another service's schema.

## What `tide` does

`tide` runs from your service repo. It reads the project's `tide.yaml`, collects the `.atl` schema files it points at, and submits them to the atlantis server. It submits schema (`apply`), previews submissions (`plan`), resyncs from server state (`pull`), inspects the version registry (`history`, `diff`, `blame`, `owners`), and reverts to a previous version (`rollback`). `diff` is tide-only — it is not available in `tidectl`.

## What `tidectl` does

`tidectl` runs on a host with direct access to the atlantis server's database and migration directory. It owns codegen, migration application, the approval flow for migrations staged by caller submissions, and server-side schema introspection (`history`, `blame`, `owners`, `rollback`).

## Why the split

Caller CI runs on every PR across every service repo. The blast radius of a buggy or malicious `tide apply` has to be bounded: no caller can drop tables or run codegen for someone else's schema. Whether `tide rollback` is permitted is controlled server-side by the `ATL_ALLOW_APPLY_MUTATION` flag — the security boundary is the server's policy, not the CLI binary. `tidectl` runs server-side and is not invoked from caller pipelines.

## Related

- [Schema as code](schema-as-code.md) — why the caller CLI is the only path that mutates schema
- [`tide` CLI reference](../reference/cli-tide.md)
- [`tidectl` CLI reference](../reference/cli-tidectl.md)
