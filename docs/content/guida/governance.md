# Release governance

manent keeps track of what is live in each environment and enforces the rules by which an artifact gets there:

- **inventory:** the live release per application and environment;
- **mandatory promotion:** only what has already passed through another environment is released;
- **approvals:** the deploy starts only after other people approve it;
- **freeze windows:** periods in which deploys are blocked, with an override for emergencies;
- **manual rollback:** go back to a previous version with one command;
- **change management:** a ticket in ServiceNow or Jira for every deploy, optionally waiting for its approval (`change` in the rule, see [Chat and change management](integrazioni.md#change-management)).

The rules live in the `governance` section of the [configuration](../operazioni/configurazione.md) and apply to CLI and API alike.

```yaml title="manent.yml"
governance:
  rules:
    - name: payments
      applications: ["pay-*"]
      environments: [prod]
      requires: [preprod]
      approvals: {required: 2, timeout: 48h}
      change: {approval: true}       # change ticket approved in the ITSM
    - name: production
      applications: ["*"]
      environments: [prod]
      requires: [preprod]
      approvals: {required: 1}
  freezes:
    - name: year-end
      reason: financial year close
      applications: ["*"]
      environments: [prod]
      from: 2026-12-20T00:00:00+01:00
      to: 2027-01-07T00:00:00+01:00
    - name: weekend
      applications: ["*"]
      environments: [prod]
      cron: "0 18 * * FRI"
      duration: 62h
      timezone: Europe/Rome
```

For each deploy **the first rule** (in order) whose `applications` and `environments` match applies. Without rules there are no constraints. [Dry-runs](dry-run.md) are not subject to the rules, because they do not change environments.

## Inventory { #inventario }

The inventory is rebuilt from the deploy history: for each application and environment, the last deploy that completed successfully.

```sh
manent inventory [-a myapp] [-e prod]
```

```json
[{
  "application": "myapp",
  "environment": "prod",
  "current": {
    "id": "7c0e…", "state": "SUCCESS", "artifact": "myapp", "version": "1.4.0",
    "sha256": "2d27fbdf…", "kind": "promotion", "source": "b3a1…",
    "requested_by": "token:ci", "updated_at": "2026-10-10T09:12:00Z"
  },
  "last_failure": {"id": "91aa…", "state": "ERROR", "version": "1.5.0", "rollback": "completed"}
}]
```

| Field | Meaning |
|---|---|
| `current` | Last successful deploy: version, `sha256` of the artifact actually released, who requested it. |
| `kind` | `deploy`, `promotion` or `rollback`. For promotions and rollbacks `source` is the deploy they derive from. |
| `last_failure` | Present if a failed or cancelled deploy came after `current`. With `rollback: completed` the environment went back to `current`; otherwise its state must be checked. |
| `targets`, `out_of_sync` | State read from the targets, if reconciliation is enabled: see [real state of the targets](#stato-reale). |
| `drift` | `true` if `last_failure` is a [wave rollout](ondate.md) stopped after updating only some regions or AZs (`rollout_progress`, e.g. `2/4`): the environment runs different versions. |

The complete history of an environment is read with the deploy list:

```sh
manent list -a myapp -e prod --state SUCCESS -n 50
```

!!! note
    The inventory is rebuilt from the last 10,000 deploys when the server starts and every 10 minutes, and in the meantime it follows new and running deploys. An application that has not been released for much longer may not appear: in that case the target system is the source of truth.

## Real state of the targets { #stato-reale }

The inventory says what manent released. With `reconcile.interval` (see [configuration](../operazioni/configurazione.md#reconcile)) the workers also read what is really on the targets, and report those changed outside manent: a manual deploy, a rollback by hand, a node rebuilt from an old image.

- At every interval, for every application and environment with a release in service, the worker takes the request of the deploy in `current`, merges it with the **current** environment configuration (so it sees targets added or removed after the deploy) and asks the plugin for the release in service **one target at a time**, with `reconcile.concurrency` reads together.
- Environments with a running deploy, or waiting for an approval within a rollout, are skipped: their targets are changing.
- The read changes nothing: it is the same one rollback does before the deploy (`CurrentRelease`). It needs a plugin that supports it: `remote-copy` with `layout: releases`, `container`, `kubernetes`, `ecs`, `lambda`, `cloudrun`, `jboss` and the external plugins that declare `current_release`.
- One worker at a time reads (lease `reconcile`). With `dbos.region_routing` every region has its own lease and its workers read only the targets of their region; targets without a region are read by a worker without a region.

The inventory reports the result in `targets`, and `out_of_sync: true` if at least one target is `drifted` or `missing`:

```json
{
  "application": "myapp", "environment": "prod",
  "current": {"id": "7c0e…", "version": "1.4.0"},
  "out_of_sync": true,
  "targets": {
    "checked_at": "2026-10-17T09:20:00Z", "deployment": "7c0e…", "version": "1.4.0",
    "targets": [
      {"host": "web-1", "region": "eu-1", "az": "eu-1a", "version": "1.4.0", "state": "in-sync"},
      {"host": "web-2", "region": "eu-1", "az": "eu-1b", "version": "1.3.2", "state": "drifted"}
    ]
  }
}
```

| `state` | Meaning |
|---|---|
| `in-sync` | The target runs the version of `current`. |
| `drifted` | The target runs another version. |
| `missing` | The target has no release (for example a new node, never released to). |
| `standby` | In [percentage rollouts](traffico.md) with the environment's router: the slot without traffic, with the replaced version. Not an anomaly. |
| `unknown` | The target cannot be read (`detail` reports the error, for example SSH unreachable). |

A reading counts only for the deploy it was made with: after a new deploy the inventory shows no `targets` until the next reconciliation has compared the targets with the new version. If the whole environment cannot be read (plugin without `CurrentRelease`, invalid environment configuration) `targets.error` explains why.

```sh
manent inventory --out-of-sync      # only environments with targets changed outside manent
```

Every target that becomes `drifted` or `missing` produces a warning log (`target cambiato fuori da manent`, with host, expected and found version), also sent to the SIEM; coming back in sync an informational log. The `manent_targets` metric counts the targets per state (see [Observability](../operazioni/osservabilita.md)). In the web interface the inventory cell lists the changed targets.

## Promotion { #promozione }

`requires` lists the environments in which **the same artifact** must already have been released successfully: same application, same name and same version and, if the descriptor sets `artifact.sha256`, same hash. A deploy that does not respect the rule is rejected at enqueue time (`422`):

```text
richiesta non valida: myapp 2.0.0 non risulta rilasciato con successo in preprod: va promosso da lì (governance)
```

The simplest way to respect it is to promote:

```sh
manent promote -a myapp --from preprod --to prod [--version 1.4.0] [--deploy-file deploy-prod.yml]
```

Promotion:

1. finds in `--from` the last successful deploy (or the one of the given version);
2. takes its descriptor, changes the environment and pins `artifact.sha256` to the recorded hash: the `qa` step checks that exactly the same file is released;
3. **does not copy `deploy.targets`**, which belong to each environment: they come from the destination [environment configuration](configurazione-ambiente.md) or from `--deploy-file`, a YAML file with the `deploy` section (`policy`, `plugin`, `targets`, `params`) that replaces the source one;
4. enqueues the new deploy with `kind: promotion`, subject like any other to policies, approvals and freezes.

| Permission | Environment |
|---|---|
| `deployment.read` | source |
| `deployment.submit` (or `deployment.dry-run` with `--dry-run`) | destination |

## Approvals { #approvazioni }

With `approvals.required` the deploy does not run immediately: it stays `AWAITING_APPROVAL` until it receives the required number of approvals from **different identities**.

| Key | Default | Description |
|---|---|---|
| `required` | `0` | Approvals needed. |
| `allow_self` | `false` | Allows approval by whoever requested the deploy. |
| `timeout` | `72h` | When it expires the request becomes `EXPIRED` and the deploy does not run. |
| `when` | `before` | `before`: approvals before the deploy. `rollout`: no approval before the deploy, the rule's approvals are asked at the approval points of the rollout (see below). |

With a [wave rollout](ondate.md#approvazioni-e-freeze) or a [percentage rollout](traffico.md) that has `approvalsBetween`, a rule with `approvals` asks for approvals twice by default: before the deploy and at every point of the rollout. With `when: rollout` the first wave (or the candidate at weight 0) starts without approval, and the approvals of the rule (number, `allow_self`, `timeout`) are asked only at the points listed in `approvalsBetween`. A deploy covered by such a rule must have a rollout that actually pauses: an `approvalsBetween` entry after a wave that is not the last one, or a step of the [percentage rollout](traffico.md). Otherwise it is rejected at submission (`422`), or fails in the `config` step, before touching any target, if the environment configuration changed in the meantime. The first percentage release has no stable version and skips the intermediate steps: the approval is then asked before any traffic reaches the candidate. Change approval in the ITSM (`change.approval`) is still asked before the deploy.

```sh
manent deploy -f prod.yml
# deploy awaiting approval: manent approve 7c0e…

manent list --state AWAITING_APPROVAL       # pending requests
manent approve 7c0e… -m "CHG-1234"          # anyone with the deployment.approve permission
manent reject 7c0e… -m "change missing"     # one rejection closes the request
```

The deploy status reports the decisions:

```json
{
  "id": "7c0e…", "state": "AWAITING_APPROVAL", "requested_by": "token:ci",
  "approval": {
    "status": "pending", "required": 2, "expires_at": "2026-10-13T09:00:00Z",
    "decisions": [{"approver": "oidc:anna", "approve": true, "comment": "CHG-1234", "time": "…"}]
  }
}
```

| State | Meaning |
|---|---|
| `AWAITING_APPROVAL` | Waiting for approvals. |
| `REJECTED` | Rejected: the deploy does not run. |
| `EXPIRED` | Not approved within the timeout. |
| `CANCELLED` | Withdrawn with `manent cancel`. |

Once the approvals are reached, the deploy is enqueued **with the same ID** and proceeds like the others. A [wave rollout](ondate.md#approvazioni-e-freeze) can also ask for approvals between one wave and the next (`approvalsBetween`): they are given with the same `manent approve`. If it was scheduled with `schedule.at` it still waits for the time.

Rules:

- **Permission.** Approving and rejecting require `deployment.approve` on the application and environment, granted by the predefined `approver` role. The `deployer` and `operator` roles do not include it.
- **Self-approval.** Whoever requested the deploy cannot approve it (`403`), unless `allow_self: true`. They can, however, reject it to withdraw it.
- **One decision per person.** A second decision by the same identity is refused.
- **Recurring schedules.** If the environment requires approvals when the schedule is created, every run waits for approval.
- **Rule at request time.** The required approvals are those valid when the deploy is enqueued.
- **Separate queue.** The wait happens on a dedicated queue (`<dbos.queue>-approvals`): it takes no worker slots and does not block other deploys of the same environment.

Every decision is recorded in the audit log (`deployment.approve`, `deployment.reject`), as is the final outcome (`approval.finish`).

## Freeze windows { #finestre-di-freeze }

During a freeze window real deploys on the application and environment are blocked.

| Key | Description |
|---|---|
| `name`, `reason` | Name and reason, reported in the error message. |
| `applications`, `environments` | Patterns with `*`. |
| `regions` | Optional: limits the window to the passes of [wave rollouts](ondate.md#approvazioni-e-freeze) in these regions (patterns with `*`). A window with `regions` does not block enqueuing: the rollout pauses before the region and resumes once the window closes. |
| `from`, `to` | Single window (RFC 3339). |
| `cron`, `duration`, `timezone` | Recurring window: each occurrence starts at `cron` (5 fields) and lasts `duration`. |

The freeze is checked twice:

- **at enqueue time:** an immediate deploy, or one scheduled with `schedule.at` inside a window, is rejected with `422`;
- **at start on the worker,** in the `config` step: a deploy enqueued earlier (for example by a schedule or approved late) that starts during a window fails immediately, without retries.

```sh
manent freezes [-a myapp] [-e prod]     # active windows and next occurrences
```

### Emergency override

An urgent deploy can bypass the freeze by stating the reason, for example the incident number:

```yaml title="descriptor"
freezeOverride: "INC-4711: VAT calculation fix"
```

or `--freeze-override "INC-4711 …"` with `promote` and `rollback`. The override:

- requires the `deployment.freeze-override` permission, granted only by `admin` or a dedicated role;
- is recorded in the audit log (`comment` field) and in the output of the `config` step;
- does not exempt from approvals;
- is not allowed with `schedule.cron`, because it applies to a single deploy.

## Manual rollback { #rollback-manuale }

[Automatic rollback](rollback.md) steps in when a deploy fails. When instead a successful deploy turns out to be faulty, you go back to a previous version:

```sh
manent rollback -a myapp -e prod [--to 1.3.2] [--freeze-override "INC-…"]
```

Manual rollback:

1. picks from the environment's history the last successful deploy of a version different from the live one (or the one given by `--to`). If, after the last successful deploy, a deploy failed leaving the targets changed (no automatic rollback, a failed rollback, or a rollout stopped after the first stage), the live version is no longer guaranteed: the rollback restores the version of the last successful deploy, which can also be given with `--to`;
2. releases it again with its descriptor and its `sha256`, as a new deploy with `kind: rollback`;
3. is allowed only for artifact types for which `rollback.artifact_types.<type>.allowed` is `true`. **Migrations** are excluded by default: releasing the previous version of a migration does not undo the schema changes.

It requires the `deployment.rollback` permission (`deployer` and `operator` roles) and is subject to approvals and freezes like any deploy. The plugin receives a normal deploy of the previous version: the artifact is downloaded, verified and copied to the targets again, so it must still be available at the source.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/inventory?application=&environment=` | Inventory. |
| `POST` | `/api/v1/promotions` | Promotion. |
| `POST` | `/api/v1/rollbacks` | Manual rollback. |
| `POST` | `/api/v1/deployments/{id}/approve` | Approve. |
| `POST` | `/api/v1/deployments/{id}/reject` | Reject. |
| `GET` | `/api/v1/freezes?application=&environment=` | Freeze windows. |

See [HTTP API](api.md#governance).
