# atlantis caller CI templates

Two GitHub Actions workflows that wire a caller repo to your atlantis
server. Drop them into your caller repo's `.github/workflows/`, configure
four secrets + one variable, and you have schema-as-code with required
review for every change.

## Files

| File | Triggers on | Runs |
|---|---|---|
| `atlantis-plan.yml` | PR opened / synchronize (touching `**/*.atl`) | `tide plan` — read-only impact report; non-zero exit on breaking → blocks merge via branch protection |
| `atlantis-apply.yml` | Push to `main` (touching `**/*.atl`) | `tide apply` — the only path schema reaches prod |

## Setup, step by step

### 1. Register two callers in the atlantis console

You need two distinct identities so the apply path can't be triggered
from anything but `main` CI:

- **`ci-<your-caller>-readonly`** — `can_mutate=false`. Used by the plan
  workflow on every PR.
- **`ci-<your-caller>-apply`** — `can_mutate=true`. Used by the apply
  workflow only.

Register both via the console's **Callers → Add caller** dialog. Issue a
cert for each (key icon on the row).

### 2. Drop the workflow files into your repo

```sh
mkdir -p .github/workflows
cp /path/to/atlantis/docs/examples/atlantis-plan.yml  .github/workflows/
cp /path/to/atlantis/docs/examples/atlantis-apply.yml .github/workflows/
```

Edit both files and replace the `ORG=rachitkumar205` line with your
atlantis fork's GitHub org so the `curl` URL points at the right
releases. (Pin the tide binary you'll install; don't track `latest`.)

### 3. Configure repository secrets

Settings → Secrets and variables → Actions → **New repository secret**:

| Secret name | Value | Used by |
|---|---|---|
| `ATL_ENDPOINT` | `atlantis.yourco.com:443` (host:port) | both |
| `TIDE_TLS_CA_PEM` | contents of `ca.crt` from the cert bundle | both |
| `TIDE_TLS_CERT_PEM` | contents of `client.crt` from the **readonly** bundle | plan only |
| `TIDE_TLS_KEY_PEM` | contents of `client.key` from the **readonly** bundle | plan only |

For the apply workflow, the simplest split is to create a separate
**environment** in GitHub (Settings → Environments → New environment →
"prod-apply") and put the apply cert there:

| Environment secret (in `prod-apply`) | Value |
|---|---|
| `TIDE_TLS_CERT_PEM` | contents of `client.crt` from the **apply** bundle |
| `TIDE_TLS_KEY_PEM` | contents of `client.key` from the **apply** bundle |

Then add `environment: prod-apply` under the `apply` job in
`atlantis-apply.yml`. This lets you require an environment approval
before any apply ever runs — useful as a backstop while you're new to the
flow.

### 4. Configure the tide version

Settings → Secrets and variables → Actions → **Variables** tab →
**New repository variable**:

| Variable | Value |
|---|---|
| `TIDE_VERSION` | a release tag like `v0.4.0` |

### 5. Add branch protection

Settings → Branches → Add rule for `main`:

- ✅ Require status checks to pass before merging
- ✅ Require `tide plan` to succeed (it'll appear after the first PR run)
- ✅ Do not allow bypass

That's it. Open a PR that edits a `.atl` file — `tide plan` runs, the
impact report appears in the CI log, breaking changes block merge. Merge
to `main` → `tide apply` runs.
