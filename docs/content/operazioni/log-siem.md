# Logs and SIEM

Every manent component writes to a single `slog` logger, which sends each record to two destinations:

- the **application log** on stderr, in `text` or `json` format (`logging.format`);
- the **SIEM**, if `siem.enabled` is `true`: one RFC 5424 message per record, over UDP or TCP (octet counting, RFC 6587).

```yaml
logging:
  level: info
  format: json
siem:
  enabled: true
  network: tcp
  address: siem.example.com:6514
```

The same logger is passed to DBOS, to plugins and to external plugin processes, so their logs reach the SIEM too.

Logs emitted during steps are also saved with the deploy and can be read with `manent logs <id>` or through the API, without access to the SIEM: see [Observability](osservabilita.md#log-e-timeline-di-un-deploy).

## RFC 5424 format

| Field | Value |
|---|---|
| `PRI` | Facility `local0`, severity from the log level (`error`, `warning`, `info`, `debug`) |
| `TIMESTAMP` | Time of the record |
| `HOSTNAME` | Worker hostname |
| `APP-NAME` | `app.name` |
| `PROCID` | Process PID |
| `MSGID` | `-` |
| `STRUCTURED-DATA` | The record's attributes in the SD-ID `manent@32473` (e.g. `deployment`, `plugin`, `step`) |
| `MSG` | The message |

An unreachable SIEM does not block manent: the send error is written to the application log and the connection is reopened on the next send.

## Useful attributes

| Attribute | Where it appears |
|---|---|
| `deployment` | All logs related to a deploy |
| `component` | `engine`, `worker`, `step`, `api`, `dbos`, `callback`, `plugin-process` |
| `plugin` | Logs of the `deploy` step and of plugins |
| `executor` | Worker logs |
| `dry_run` | Enqueuing and dry-run checks |
| `delivery` | Callback delivery |

## External plugin logs

External plugins write logs to stderr in hclog-compatible JSON. manent forwards them to its own logger with the original level, message and attributes. See [External plugins](../plugins/esterni.md#log).
