# CLI

manent has two binaries:

| Binary | For whom | What it does |
|---|---|---|
| `manent` | Developers, release managers, CI pipelines | Client: reads YAML descriptors and talks **only to the server API**. It contains no engine, plugins or database drivers. |
| `manent-server` | Platform | API, workers and database operations. See [manent-server](../operazioni/server.md). |

This page describes the client.

```sh
manent [-c <client.yml>] [--server <url>] [--token-file <file>] [--ca-file <pem>] <command> [options]
```

## Client configuration { #configurazione-del-client }

Every command goes through the API: the server's [authentication, permissions and audit](../operazioni/accesso.md) always apply.

```yaml title="~/.config/manent/client.yml"
server: https://manent.example.com
token_file: ~/.config/manent/token    # OIDC or static token
ca_file: /etc/ssl/certs/corporate-ca.pem   # only if the server's CA is not among the system ones
timeout: 60s                          # per request
```

| Setting | Option | Variable | File | Default |
|---|---|---|---|---|
| Configuration file | `-c`, `--config` | `MANENT_CLIENT_CONFIG` | | `$XDG_CONFIG_HOME/manent/client.yml` or `~/.config/manent/client.yml`, if it exists |
| Server | `--server` | `MANENT_SERVER` | `server` | `http://127.0.0.1:8080` (local `manent-server serve`) |
| Token | | `MANENT_TOKEN` | | |
| Token file | `--token-file` | `MANENT_TOKEN_FILE` | `token_file` | |
| Server CA | `--ca-file` | `MANENT_CA_FILE` | `ca_file` | system CAs |

The option wins over the variable, which wins over the file. For the token: `--token-file` (or `MANENT_TOKEN_FILE`), then `MANENT_TOKEN`, then the file's `token_file`. The token is never passed as an option, because it would stay in the shell history.

```sh
export MANENT_SERVER=https://manent.example.com
export MANENT_TOKEN=…
manent deploy -f release.yml
```

In a CI pipeline you can use the `ghcr.io/joeg-ita/manent` image, which contains only the client.

## Deploy

### `deploy`

Enqueues a deploy from a YAML descriptor and, by default, waits for it to finish.

```sh
manent deploy -f examples/descriptor.yml [--no-wait] [--timeout 30m] [--dry-run]
```

| Option | Description |
|---|---|
| `-f`, `--file` | YAML descriptor (required). |
| `--no-wait` | Prints the ID and exits immediately, without waiting. |
| `--timeout` | Maximum wait (e.g. `30m`). When it expires the command exits, but the deploy continues. |
| `--dry-run` | Runs the deploy as a [simulation](dry-run.md). |
| `--after <id>` | Starts the deploy only after the deploy `<id>` has succeeded (repeatable): see [Deploy chains](catene.md). |
| `--import <name>=<id>` | Reads the exports of the deploy `<id>` as the import `<name>` of the descriptor and adds `<id>` to `after` (repeatable): see [Exports and imports](catene.md#export-import). |
| `--id`, `--version`, `--source`, `--sha256` | Replace the ID and the artifact fields of the descriptor (the pipeline gives the version). |
| `--outputs <file>`, `--summary <file>`, `--ci` | `key=value` outcome and Markdown summary for CI: see [CI and infrastructure as code](ci-iac.md). |

Scheduled deploys (`schedule` in the descriptor) and those [awaiting approval](governance.md#approvazioni) are not waited for: the command prints the state and exits.

### `validate`

Checks the descriptor on the server with the same checks as enqueuing (descriptor policy, environment configuration, callbacks, artifact source, rollback) and shows the plan and the effective deploy, i.e. the descriptor merged with the environment configuration. It enqueues nothing and does not contact secrets, artifact or targets. Requires the `deployment.read` permission.

```sh
manent validate -f examples/descriptor.yml
```

The output also lists the names of the declared secrets, the [governance](governance.md) rule that applies and any active freeze. If the environment configuration cannot be reached, the server reports it in `warnings` and the check is left to the worker. To check a manent configuration before installing it there is [`manent-server validate`](../operazioni/server.md#validate).

### `env validate`

Checks a candidate environment configuration before it reaches the configuration system, merging it with an example descriptor (default: the request of the deploy in service). See [CI and infrastructure as code](ci-iac.md#env-validate).

```sh
manent env validate -a <application> -e <environment> -f prod.yml [-d descriptor.yml]
```

### `status`

```sh
manent status <id>
```

Prints state, dates and, for finished deploys, the result of each step or the error.

### `list`

```sh
manent list [-s <schedule>] [-a <application>] [-e <environment>] [--state SUCCESS] [-n 20]
```

Lists deploys from the most recent, including those awaiting approval or rejected. With `--schedule` it shows only the runs of a recurring schedule. Filters (and permissions) do not reduce the number of results: the list keeps going through older deploys up to `-n`.

### `cancel`

```sh
manent cancel <id>
```

A queued, scheduled or awaiting-approval deploy is removed. A running deploy stops at the start of the next step: the step in progress still completes.

## Governance

See [Release governance](governance.md).

```sh
manent inventory [-a <application>] [-e <environment>] [--out-of-sync]
manent promote -a <application> --from <environment> --to <environment> [--version <v>] [--deploy-file <yaml>]
manent rollback -a <application> -e <environment> [--to <version>]
manent approve <id> [-m <comment>]
manent reject <id> -m <reason>
manent freezes [-a <application>] [-e <environment>]
manent approvals                # deploys waiting for a decision that you can take
manent whoami                   # your identity on the server and your roles
```

`promote` and `rollback` also accept `--id`, `--dry-run`, `--freeze-override <reason>`, `--no-wait` and `--timeout`, with the same meaning as for `deploy`.

### Artifacts

See [Applications with several artifacts](artefatti.md).

```sh
manent list|inventory|dora -A <artifact>
manent promote -a <application> --from <env> --to <env> -A <artifact>
manent rollback -a <application> -e <env> -A <artifact>      # required with more than one artifact in the environment
manent env validate -a <application> -e <env> -A <artifact> -f <yaml>
manent freeze add --name <name> -e <env> -A <artifact pattern> --for 4h
manent-server -c manent.yml admin rename-artifact -a <application> --from <name> --to <name> [--dry-run]
```

### Tenants

See [Tenants](tenant.md).

```sh
manent tenants list [-a <application>] [--selector <tenant selector>]
manent tenants show <tenant>                             # applications with their tags, queue
manent list|inventory|activity|dora -t <tenant>          # dora --by-tenant: metrics per tenant
manent list|inventory --selector <application selector>  # e.g. criticality=high, tenant=payments
manent freeze add ... --selector <application selector>
```

### Freezes and policies

See [Policies in Rego](policy.md).

```sh
manent freeze add --name <name> -e <environment> [-a <application>] [--region <r>] (--for <duration> | --until <RFC 3339>) [--from <RFC 3339>] [--reason <text>] [--hard]
manent freeze delete <name>
manent freeze list

manent policy list
manent policy show <revision|current>
manent policy pull <revision|current> -d <dir>
manent policy push -d <dir> [-m <comment>]
manent policy diff <revision> <revision>
manent policy eval <revision|current> -f <yaml> [--phase submit|validate|start|stage|tick] [--kind deploy|promotion|rollback]
manent policy impact <revision> [--since 7d]
manent policy access <revision|current> --subject <method:name> [--group <g>]... [--claim k=v]... [--compare]
                     [--action <action> -a <app> -e <env> [-A <artifact>] [-f <yaml>]] [--json]   # rbac.engine: opa
manent policy approve <revision>         # activates a draft
manent policy activate <revision>        # back to a revision already active in the past
```

## Logs, timeline and metrics

```sh
manent logs [-f] [--json] <id>         # deploy log; -f follows it until completion
manent steps <id>                      # recorded steps with times, duration, outputs and errors
manent dora [--since 30d] [-a <application>] [-e <environment>]
```

See [Observability](../operazioni/osservabilita.md).

## Composite releases

```sh
manent release submit -f release.yml [--no-wait] [--timeout 2h]   # submits and follows the release
manent release status <id>
manent release list [-n 20]
manent release cancel <id>
```

`submit` prints a line for every change of state of a deploy (and of its rollback) and exits with an error if the release does not succeed. See [Deploy chains](catene.md#release-composte).

## Activity and workers { #attivita-e-worker }

```sh
manent activity [-a <application>] [-e <environment>] [-w 5s] [--json]   # active deploys
manent workers [-w 5s] [--json]                                           # workers, resources, queues and their work
manent queues [-w 5s] [--json]                                            # queues, limits and listening workers
```

`activity` shows the deploys that have not finished yet, grouped as **running** (with the worker that runs them, the last recorded step and the wave progress), **awaiting approval**, **waiting for dependencies** (with the deploys still awaited), **queued** (waiting for a free slot of the environment) and **scheduled** (`schedule.at`). Only deploys you can read are listed.

`workers` lists the registered workers with state (`alive`, `stale` when the heartbeat is missing, `stopped`, `unregistered` for an executor that has pending workflows but is not in the registry), region, workflow and binary versions, last heartbeat and the workflows running on each: deploys, wave steps in a region, health gates, approvals waiting for a decision. It requires the [`platform.read`](../operazioni/accesso.md#permessi-rbac) permission; workflows of applications you cannot read are only counted. A second table shows the resources each worker reports at every heartbeat (CPU used against the CPUs it may use, process memory against the container limit or the host memory, host load, free space of the workdir, deploys it runs together) and the queues it listens to.

`queues` lists the queues: the deploy queue, partitioned by application and environment, the queue of approvals and waits, which takes no deploy slot, and one queue per region with region routing. For each: workflows waiting for a slot, running and scheduled, the oldest wait, the concurrency limits and the live workers that listen to it. A queue with work and no listening worker (for example a region whose workers are all down) is the first thing to check when deploys do not start. It requires `platform.read`.

All three print a table; `--json` prints the API response, `-w` repeats the list at the given interval until Ctrl-C.

## Schedules

```sh
manent schedule list
manent schedule show <name>
manent schedule pause <name>
manent schedule resume <name>
manent schedule delete <name>
```

Schedules are created with a descriptor containing `schedule.cron`. See [Scheduled deploys](pianificazione.md).

## Version

```sh
manent version     # client version, commit and build date
```

## Exit codes

The command exits with code `0` if the operation succeeds and with a non-zero code on error, including a deploy that ended with an error when `deploy` waits for its outcome.
