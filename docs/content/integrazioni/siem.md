# SIEM and syslog

Every manent component, DBOS and the plugins included, writes to one logger that sends each record both to the application log and, with `siem.enabled`, to structured destinations: a remote collector in **RFC 5424**, the local syslog socket, or **journald** with its native protocol. Audit entries travel the same way, marked with `audit=true`, so the SIEM holds the authoritative copy of who did what.

!!! abstract "What you need"
    A syslog-capable collector (Splunk, Elastic, QRadar, Sentinel through a forwarder, rsyslog, syslog-ng…) or systemd-journald on the hosts

## 1. Send to a remote collector

```yaml title="manent.yml"
logging:
  level: info
  format: json            # application log on stderr: json or text
siem:
  enabled: true
  network: tcp            # udp | tcp (octet counting, RFC 6587) | unixgram | journald
  address: siem.example.com:6514
```

Each record becomes an RFC 5424 message with facility `local0`, the severity of the log level, `APP-NAME` from `app.name` and the record's attributes as structured data in the SD-ID `manent@32473`:

```text
<134>1 2026-10-19T17:26:31.094Z worker-0 manent 7 - [manent@32473 component="step" deployment="49bef919-…" step="deploy" plugin="remote-copy"] deploy completato
```

An unreachable collector never blocks manent: the error goes to the application log and the connection is reopened on the next record. Over UDP each message is limited to 60 KB, shortened rather than lost.

## 2. Collector recipes

=== "rsyslog"

    ```text title="/etc/rsyslog.d/10-manent.conf"
    module(load="imtcp")
    input(type="imtcp" port="6514" ruleset="manent")
    ruleset(name="manent") {
      action(type="omfile" file="/var/log/manent/manent.log" template="RSYSLOG_SyslogProtocol23Format")
      # forward to the SIEM, e.g. action(type="omfwd" target="siem.example.com" port="514" protocol="tcp")
    }
    ```

=== "Splunk"

    Create a **TCP data input** on port 6514 with source type `syslog` (or the RFC 5424 add-on's type), or let a universal forwarder or Splunk Connect for Syslog receive it. Extract the structured data fields (`deployment`, `step`, `audit`…) to search by deploy.

=== "Elastic"

    Use the **Syslog** input of Elastic Agent or Filebeat in `rfc5424` format on TCP 6514; structured data arrive as fields under `log.syslog.structured_data`.

TLS to the collector is usually handled by a local forwarder (rsyslog, syslog-ng, the Splunk forwarder) listening on localhost.

## 3. Local journal and several destinations

```yaml title="manent.yml"
siem:
  enabled: true
  network: tcp
  address: siem.example.com:6514
  destinations:
    - network: journald            # attributes become MANENT_<NAME> fields
```

```sh
journalctl SYSLOG_IDENTIFIER=manent MANENT_DEPLOYMENT=49bef919-2df2-47f3-bbec-3287a72de817
journalctl SYSLOG_IDENTIFIER=manent MANENT_AUDIT=true -o json-pretty
```

All destinations are active together, and an error on one does not stop the others.

## 4. What to search for

| Search | Finds |
|---|---|
| `audit=true` | Every audit entry: submissions (allowed and denied), approvals, cancellations, freezes, policy changes, final outcomes. |
| `audit=true action="deployment.submit" outcome="denied"` | Attempts outside a role's permissions. |
| `deployment="<id>"` | Everything about one deploy, across API and workers. |
| `msg="target cambiato fuori da manent"` | Targets changed outside manent, found by reconciliation. |
| `component="worker" level=WARN` | Recovery, lost workers, chat notifications not sent. |

The log of each deploy is also kept with the deploy and readable with `manent logs <id>` without access to the SIEM; the SIEM is where you correlate it with everything else and keep it for as long as your retention policy says.

Reference: [Logs and SIEM](../operazioni/log-siem.md), [Audit](../operazioni/accesso.md#audit).
