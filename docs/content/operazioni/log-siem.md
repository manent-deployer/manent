# Logs and SIEM

Every manent component writes to a single `slog` logger, which sends each record to two destinations:

- the **application log** on stderr, in `text` or `json` format (`logging.format`);
- the **structured log destinations**, if `siem.enabled` is `true`: a remote SIEM (RFC 5424 over UDP or TCP with octet counting, RFC 6587), the local syslog socket (`/dev/log`) or journald with its native protocol. They can be used together (see [System log and journald](#log-di-sistema-e-journald)).

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

An unreachable SIEM does not block manent: the send error is written to the application log and the connection is reopened on the next send. With several destinations, an error on one does not prevent sending to the others.

Over UDP and `/dev/log` each message is a datagram of at most 60 KB: a larger message is shortened, first the attribute values over 1 KB, then the text (which ends with `… [truncated]`), instead of being lost.

## System log and journald { #log-di-sistema-e-journald }

In addition to the remote SIEM, or instead of it, structured logs can go to the machine's system log, readable with `journalctl`. All destinations are active together:

```yaml
siem:
  enabled: true
  network: tcp                    # remote SIEM (optional)
  address: siem.example.com:6514
  destinations:
    - network: journald           # native protocol: attributes become journal fields
    # - network: unixgram         # syslog socket: RFC 5424 on /dev/log
    #   address: /dev/log
```

| `network` | Default `address` | Format |
|---|---|---|
| `udp`, `tcp` | (required) | RFC 5424 to a remote collector. |
| `unixgram` | `/dev/log` (`/var/run/syslog` on macOS) | RFC 5424 on the local syslog socket. |
| `journald` | `/run/systemd/journal/socket` | systemd-journald [native protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/). |

**`journald`** is the choice for `journalctl` on systems with systemd. Every attribute of the record becomes a `MANENT_<NAME>` field, in upper case and with characters other than letters and digits replaced by `_` (`deployment` → `MANENT_DEPLOYMENT`, `step.name` → `MANENT_STEP_NAME`). In addition there are `MESSAGE`, `PRIORITY` (from the level), `SYSLOG_FACILITY=16` (`local0`), `SYSLOG_IDENTIFIER` (`app.name`) and `SYSLOG_PID`. Messages larger than a datagram go through a temporary file, as `sd_journal_send` does.

```sh
journalctl -t manent -f                                   # all manent logs
journalctl -t manent MANENT_DEPLOYMENT=shop-api-2.4.0-prod  # the logs of a deploy
journalctl -t manent -p warning --since today             # warnings and errors
journalctl -t manent -o verbose                           # with all fields
```

**`unixgram`** writes the same RFC 5424 message sent to the SIEM. It suits rsyslog or syslog-ng listening on `/dev/log`, which parse RFC 5424 and can forward it. If journald answers on `/dev/log`, it only extracts priority and facility from the message: the rest (structured data included) stays in the text and there is no `SYSLOG_IDENTIFIER`, so messages are found with `journalctl _COMM=manent-server _TRANSPORT=syslog`, not with `-t manent`.

!!! note
    If manent runs as a systemd unit, its stderr (the application log) already ends up in the journal as text, with `SYSLOG_IDENTIFIER` equal to the process name. With the `journald` destination every record appears twice: reduce the application log (`logging.level: warn`) or send stderr elsewhere (`StandardError=null` in the unit). `journald` and `unixgram` are not available on Windows.

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
