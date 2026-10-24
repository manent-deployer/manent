# SIEM e syslog

Ogni componente di manent, compresi DBOS e i plugin, scrive su un unico logger che invia ogni record sia al log applicativo sia, con `siem.enabled`, a destinazioni strutturate: un collettore remoto in **RFC 5424**, il socket syslog locale o **journald** con il suo protocollo nativo. Le voci di audit viaggiano allo stesso modo, marcate con `audit=true`, così il SIEM conserva la copia autorevole di chi ha fatto cosa.

!!! abstract "Cosa serve"
    Un collettore che accetti syslog (Splunk, Elastic, QRadar, Sentinel tramite un forwarder, rsyslog, syslog-ng…) o systemd-journald sugli host

## 1. Invia a un collettore remoto

```yaml title="manent.yml"
logging:
  level: info
  format: json            # log applicativo su stderr: json o text
siem:
  enabled: true
  network: tcp            # udp | tcp (octet counting, RFC 6587) | unixgram | journald
  address: siem.example.com:6514
```

Ogni record diventa un messaggio RFC 5424 con facility `local0`, la severità del livello di log, `APP-NAME` da `app.name` e gli attributi del record come structured data nell'SD-ID `manent@32473`:

```text
<134>1 2026-10-19T17:26:31.094Z worker-0 manent 7 - [manent@32473 component="step" deployment="49bef919-…" step="deploy" plugin="remote-copy"] deploy completed
```

Un collettore irraggiungibile non blocca mai manent: l'errore va nel log applicativo e la connessione viene riaperta al record successivo. In UDP ogni messaggio è limitato a 60 KB, accorciato invece che perso.

## 2. Ricette per i collettori

=== "rsyslog"

    ```text title="/etc/rsyslog.d/10-manent.conf"
    module(load="imtcp")
    input(type="imtcp" port="6514" ruleset="manent")
    ruleset(name="manent") {
      action(type="omfile" file="/var/log/manent/manent.log" template="RSYSLOG_SyslogProtocol23Format")
      # inoltro al SIEM, es. action(type="omfwd" target="siem.example.com" port="514" protocol="tcp")
    }
    ```

=== "Splunk"

    Crea un **TCP data input** sulla porta 6514 con source type `syslog` (o quello dell'add-on RFC 5424), oppure lascia che lo ricevano un universal forwarder o Splunk Connect for Syslog. Estrai i campi della structured data (`deployment`, `step`, `audit`…) per cercare per deploy.

=== "Elastic"

    Usa l'input **Syslog** di Elastic Agent o Filebeat in formato `rfc5424` su TCP 6514; la structured data arriva come campi sotto `log.syslog.structured_data`.

Il TLS verso il collettore lo gestisce di solito un forwarder locale (rsyslog, syslog-ng, il forwarder di Splunk) in ascolto su localhost.

## 3. Journal locale e più destinazioni

```yaml title="manent.yml"
siem:
  enabled: true
  network: tcp
  address: siem.example.com:6514
  destinations:
    - network: journald            # gli attributi diventano campi MANENT_<NOME>
```

```sh
journalctl SYSLOG_IDENTIFIER=manent MANENT_DEPLOYMENT=49bef919-2df2-47f3-bbec-3287a72de817
journalctl SYSLOG_IDENTIFIER=manent MANENT_AUDIT=true -o json-pretty
```

Tutte le destinazioni sono attive insieme, e un errore su una non ferma le altre.

## 4. Cosa cercare

| Ricerca | Trova |
|---|---|
| `audit=true` | Ogni voce di audit: invii (consentiti e negati), approvazioni, annullamenti, freeze, modifiche alle policy, esiti finali. |
| `audit=true action="deployment.submit" outcome="denied"` | Tentativi fuori dai permessi di un ruolo. |
| `deployment="<id>"` | Tutto ciò che riguarda un deploy, tra API e worker. |
| `msg="target changed outside manent"` | Target cambiati fuori da manent, trovati dalla riconciliazione. |
| `component="worker" level=WARN` | Recovery, worker persi, notifiche in chat non inviate. |

Il log di ogni deploy viene conservato anche con il deploy e si legge con `manent logs <id>` senza accesso al SIEM; il SIEM è il posto in cui correlarlo con tutto il resto e conservarlo per il tempo richiesto dalla tua policy di retention.

Riferimento: [Log e SIEM](../operazioni/log-siem.md), [Audit](../operazioni/accesso.md#audit).
