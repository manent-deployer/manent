# Scheduled deploys

The descriptor's `schedule` section schedules the deploy instead of running it immediately. All deploys, scheduled ones included, go through the same DBOS queue and respect its concurrency limits.

## Single deploy at a given time

```yaml
schedule:
  at: 2026-10-10T02:00:00+02:00    # RFC 3339
```

The deploy is enqueued immediately in state `DELAYED` and waits until the given time, when it enters the queue like a normal deploy. A time in the past is rejected, except when a descriptor is resubmitted with the same `id` as a deploy already enqueued (for example a CI job retrying after the scheduled time): in that case the existing deploy is returned with its state.

It is cancelled like any deploy, with `manent cancel <id>` or `POST /api/v1/deployments/{id}/cancel`.

## Recurring deploys

```yaml
schedule:
  cron: "0 2 * * *"         # every day at 2:00
  timezone: Europe/Rome     # optional, default: the executor's time zone
  name: myapp-nightly       # optional, default: <application>-<environment>
```

| Field | Description |
|---|---|
| `cron` | 5-field expression (minute hour day month weekday), 6 fields with leading seconds, or a descriptor such as `@daily` or `@hourly`. |
| `timezone` | IANA time zone used to interpret the expression. |
| `name` | Schedule name. |

Behaviour:

- **creation and update:** sending the descriptor creates the schedule, or updates it if one with the same name already exists. The descriptor and the plan are saved in the schedule and reused at every run;
- **run IDs:** each tick enqueues a deploy with the deterministic ID `<name>@<UTC time>`, for example `myapp-nightly@20261010T000000Z`. A tick never produces two deploys, even with several active workers. For the same reason `id` is not allowed together with `cron`;
- **environment configuration:** each run reads the one valid at run time, not at the time the schedule was created;
- **propagation of changes:** workers reload schedule changes every `dbos.schedule_poll_interval` (default 30 seconds).

```sh
manent deploy -f examples/descriptor-scheduled.yml   # create or update
manent schedule list
manent schedule show myapp-nightly
manent list --schedule myapp-nightly                  # runs of the schedule
manent schedule pause myapp-nightly
manent schedule resume myapp-nightly
manent schedule delete myapp-nightly
```

!!! note "Concurrency"
    The schedule tick enqueues the deploy without waiting for it, so it does not take a slot of the deploy queue. The enqueued deploy respects `environment_concurrency` like the others: if a deploy of the same environment is running, it waits.
