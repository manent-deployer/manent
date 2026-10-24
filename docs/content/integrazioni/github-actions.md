# GitHub Actions

The repository ships a composite action, `manent-deployer/manent/integrations/github`, that deploys with manent, validates a descriptor or checks a candidate environment configuration. It installs the client if the runner does not have it, waits for the outcome, fails the job when the deploy fails, and publishes the result as step outputs and in the job summary.

!!! abstract "What you need"
    A manent server reachable from the runners (GitHub-hosted runners need a public address; otherwise use self-hosted runners in your network) · a token for CI ([static token](../operazioni/accesso.md) in `auth.tokens_file`)

## 1. Token and permissions

```sh
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | sha256sum         # into manent's tokens file
gh secret set MANENT_TOKEN --body "$TOKEN"
```

```yaml title="/etc/manent/tokens.yml"
tokens:
  - {name: gh-shop, sha256: "<sha256>", groups: [ci], expires_at: 2027-06-30T00:00:00Z}
```

```yaml title="manent.yml"
rbac:
  bindings:
    - {subjects: ["token:gh-shop"], role: deployer, applications: [shop], environments: [test, staging]}
    - {subjects: ["token:gh-shop"], role: tester, applications: [shop], environments: [prod]}   # dry-run only
```

One token per repository keeps the audit trail readable (`requested_by: token:gh-shop`) and limits what a leaked token can do.

## 2. Deploy on every tag

```yaml title=".github/workflows/release.yml"
on:
  push:
    tags: ["v*"]

jobs:
  deploy-test:
    runs-on: ubuntu-latest
    environment: test
    steps:
      - uses: actions/checkout@v4
      - id: manent
        uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/test.yml
          version: ${{ github.ref_name }}
          source: https://nexus.example.com/repository/releases/shop/shop-${{ github.ref_name }}.tgz
          id: gh-${{ github.run_id }}
          timeout: 30m
      - run: echo "deploy ${{ steps.manent.outputs.deployment-id }} ended ${{ steps.manent.outputs.state }}"
```

| Input | Default | Meaning |
|---|---|---|
| `server`, `token` | | manent address and token (required). |
| `command` | `deploy` | `deploy`, `validate` (descriptor) or `env-validate` (candidate environment configuration). |
| `descriptor` | | YAML descriptor in the repository. For `env-validate` it is the optional example descriptor. |
| `version`, `source`, `sha256` | | Replace the artifact fields of the descriptor. |
| `id` | | Deploy ID, the idempotency key. |
| `dry-run` | `false` | Run a [dry-run](../guida/dry-run.md) on a worker. |
| `wait`, `timeout` | `true`, `30m` | Wait for the end (approvals included) up to `timeout`; the deploy goes on after the timeout. |
| `after` | | IDs of deploys that must succeed first, space-separated. |
| `application`, `environment`, `env-config` | | For `env-validate`. |
| `ca-file` | | CA of the server, if private. |

Outputs: `deployment-id`, `state` and `version`.

!!! tip "Choosing the `id`"
    `gh-${{ github.run_id }}` makes **Re-run jobs** return the deploy already done instead of releasing again. Use `gh-${{ github.run_id }}-${{ github.run_attempt }}` when a re-run should deploy again.

## 3. Dry-run on pull requests

```yaml title=".github/workflows/pr.yml"
on: pull_request

jobs:
  manent-dry-run:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          descriptor: deploy/prod.yml
          dry-run: true
```

A worker resolves the secrets, downloads and verifies the artifact and checks the targets of production without changing them. With `command: validate` the check stays on the API server and does not touch secrets, artifact or targets.

## 4. Promote to production

The action covers `deploy`, `validate` and `env-validate`. For a promotion, install the client and call it directly:

```yaml title=".github/workflows/release.yml (continued)"
  promote-prod:
    needs: deploy-test
    runs-on: ubuntu-latest
    environment: production                   # GitHub reviewers, if you use them, come first
    steps:
      - uses: actions/setup-go@v5
        with: {go-version: "1.27"}
      - run: go install github.com/manent-deployer/manent/cmd/manent@v0.1.0
      - run: manent promote -a shop --from test --to prod --version "${{ github.ref_name }}" --timeout 4h
        env:
          MANENT_SERVER: https://manent.example.com
          MANENT_TOKEN: ${{ secrets.MANENT_PROMOTE_TOKEN }}
```

The token needs `deployment.read` on `test` and `deployment.submit` on `prod`. manent's own [approvals](../guida/governance.md#approvazioni) still apply: the job waits, up to `--timeout`, for a person to approve in manent, Slack or the web interface.

## 5. Check environment configuration changes

In the repository that holds the environment configuration:

```yaml title=".github/workflows/envconfig.yml"
on:
  pull_request:
    paths: ["envconfig/**"]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: manent-deployer/manent/integrations/github@v0.1.0
        with:
          server: https://manent.example.com
          token: ${{ secrets.MANENT_TOKEN }}
          command: env-validate
          application: shop
          environment: prod
          env-config: envconfig/shop/shop/prod.yml
```

The server merges the candidate with the request of the deploy in service and runs every submission check (merge, descriptor policy, rollout and traffic plan, plugins). It writes nothing; the permission needed is `deployment.read`.

## Private CA and self-hosted runners

With a server certificate from a corporate CA, commit or provision the CA on the runner and pass it with `ca-file: /etc/ssl/certs/corp-ca.pem`. On self-hosted runners with the client already installed, the action uses it and skips the Go setup.

Reference: [CI and infrastructure as code](../guida/ci-iac.md#github-action).
