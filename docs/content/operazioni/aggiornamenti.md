# Versions and upgrades

## manent versions

manent follows [Semantic Versioning](https://semver.org/). The changes of each version are in [`CHANGELOG.md`](https://github.com/joeg-ita/manent/blob/main/CHANGELOG.md).

A **major** version (or, before 1.0, a **minor** one) can introduce incompatible changes to:

- the HTTP API and the JSON format of responses;
- the deploy descriptor (`apiVersion`) and the `manent.yml` configuration;
- the [external plugin](../plugins/esterni.md) protocol (`ProtocolVersion`) and the `pkg/plugin` package;
- the **workflow version** (see below).

**Patch** versions fix bugs without changing anything in this list.

```sh
manent version
```

## Two different versions

| | Where | What it means |
|---|---|---|
| Binary version | `manent version`, `manent_build_info` metric | The installed manent release. |
| Workflow version | `app.version` in `manent.yml` | DBOS workflow compatibility: a worker resumes **only** the workflows of its own version. |

DBOS records with each workflow the version of the application that started it and, after a crash, resumes it by replaying the steps already recorded. If the workflow code changes incompatibly (steps in a different order, different input), resuming with the new code would give wrong results. Therefore:

- **`app.version` must not be changed at every manent release**, but only when the release notes say so ("workflow version changed");
- a worker with a new `app.version` neither picks up nor resumes the workflows of the previous version: they must be completed before the upgrade or migrated.

## Upgrading manent

### Release without a workflow version change

This is the normal case. API and workers are updated one at a time (rolling update, the Helm chart's default behaviour):

1. each worker receives `SIGTERM`, stops picking up deploys and waits for those in progress up to `dbos.drain_timeout` ([drain](affidabilita.md#arresto-ordinato-drain));
2. it restarts with the new binary and the same `executor_id`, and resumes any deploys left pending.

### Release with a new workflow version

1. **Drain.** Pause the schedules that fire during the upgrade (`manent schedule pause`), then wait for running deploys to finish (`manent list --state PENDING`).
2. **Upgrade** the workers and set the new `app.version`. Deploys enqueued after the upgrade use the new version.
3. **Migrate** what is left of the previous version, for example deploys still queued or pending approvals:

    ```sh
    manent-server admin executors                                   # pending per version
    manent-server admin recover --from-version 0.1.0 --dry-run      # what would be migrated
    manent-server admin recover --from-version 0.1.0                # to app.version
    ```

    Each workflow is cancelled and brought to the new version with the **same ID**. Completed steps stay recorded and the workflow restarts from the next one. With `--from-start` it restarts from scratch instead, rerunning all steps, for example if the release notes require it. The command refuses the migration if a worker of the previous version still appears alive.

4. Resume the schedules.

!!! note "Migration limits"
    - Deploys scheduled with `schedule.at` (`DELAYED`) are not migrated: they must be cancelled and rescheduled.
    - Decisions sent to an approval after its worker was lost and not yet processed must be repeated after the migration.

## Rolling back an upgrade

If the workflow version has not changed, go back to the previous binary with another rolling update. Otherwise also restore `app.version` and migrate back, with `manent-server admin recover --from-version`, the workflows started with the new version.
