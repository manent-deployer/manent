# Log e SIEM

Ogni componente di manent scrive su un unico logger `slog`, che invia ogni record a due destinazioni:

- il **log applicativo** su stderr, in formato `text` o `json` (`logging.format`);
- le **destinazioni dei log strutturati**, se `siem.enabled` è `true`: un SIEM remoto (RFC 5424 via UDP o TCP con octet counting, RFC 6587), il socket syslog locale (`/dev/log`) o journald con il protocollo nativo. Si possono usare insieme (vedi [Log di sistema e journald](#log-di-sistema-e-journald)).

```yaml
logging:
  level: info
  format: json
siem:
  enabled: true
  network: tcp
  address: siem.example.com:6514
```

Lo stesso logger viene passato a DBOS, ai plugin e ai processi dei plugin esterni, quindi anche i loro log arrivano al SIEM.

I log emessi durante gli step sono salvati anche con il deploy e si leggono con `manent logs <id>` o dall'API, senza accedere al SIEM: vedi [Osservabilità](osservabilita.md#log-e-timeline-di-un-deploy).

## Formato RFC 5424

| Campo | Valore |
|---|---|
| `PRI` | Facility `local0`, severità dal livello del log (`error`, `warning`, `info`, `debug`) |
| `TIMESTAMP` | Orario del record |
| `HOSTNAME` | Hostname del worker |
| `APP-NAME` | `app.name` |
| `PROCID` | PID del processo |
| `MSGID` | `-` |
| `STRUCTURED-DATA` | Gli attributi del record nello SD-ID `manent@32473` (es. `deployment`, `plugin`, `step`) |
| `MSG` | Il messaggio |

Un SIEM irraggiungibile non blocca manent: l'errore di invio viene scritto sul log applicativo e la connessione viene riaperta all'invio successivo. Con più destinazioni, l'errore di una non impedisce l'invio alle altre.

Via UDP e `/dev/log` ogni messaggio è un datagramma di al più 60 KB: un messaggio più grande viene accorciato, prima i valori degli attributi oltre 1 KB, poi il testo (che termina con `… [troncato]`), invece di andare perso.

## Log di sistema e journald { #log-di-sistema-e-journald }

Oltre al SIEM remoto, o al suo posto, i log strutturati possono andare al log di sistema della macchina, consultabile con `journalctl`. Le destinazioni sono attive tutte insieme:

```yaml
siem:
  enabled: true
  network: tcp                    # SIEM remoto (facoltativo)
  address: siem.example.com:6514
  destinations:
    - network: journald           # protocollo nativo: gli attributi diventano campi del journal
    # - network: unixgram         # socket syslog: RFC 5424 su /dev/log
    #   address: /dev/log
```

| `network` | `address` di default | Formato |
|---|---|---|
| `udp`, `tcp` | (obbligatorio) | RFC 5424 verso un collettore remoto. |
| `unixgram` | `/dev/log` (`/var/run/syslog` su macOS) | RFC 5424 sul socket syslog locale. |
| `journald` | `/run/systemd/journal/socket` | [Protocollo nativo](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/) di systemd-journald. |

**`journald`** è la scelta per `journalctl` su sistemi con systemd. Ogni attributo del record diventa un campo `MANENT_<NOME>`, in maiuscolo e con i caratteri diversi da lettere e cifre sostituiti da `_` (`deployment` → `MANENT_DEPLOYMENT`, `step.name` → `MANENT_STEP_NAME`). Oltre a questi ci sono `MESSAGE`, `PRIORITY` (dal livello), `SYSLOG_FACILITY=16` (`local0`), `SYSLOG_IDENTIFIER` (`app.name`) e `SYSLOG_PID`. I messaggi più grandi di un datagramma passano da un file temporaneo, come fa `sd_journal_send`.

```sh
journalctl -t manent -f                                   # tutti i log di manent
journalctl -t manent MANENT_DEPLOYMENT=shop-api-2.4.0-prod  # i log di un deploy
journalctl -t manent -p warning --since today             # avvisi ed errori
journalctl -t manent -o verbose                           # con tutti i campi
```

**`unixgram`** scrive lo stesso messaggio RFC 5424 inviato al SIEM. È adatto a rsyslog o syslog-ng in ascolto su `/dev/log`, che interpretano l'RFC 5424 e possono inoltrarlo. Se su `/dev/log` risponde journald, questo ricava dal messaggio solo priorità e facility: il resto (structured data compresi) resta nel testo e non c'è `SYSLOG_IDENTIFIER`, quindi i messaggi si cercano con `journalctl _COMM=manent-server _TRANSPORT=syslog`, non con `-t manent`.

!!! note
    Se manent gira come unit systemd, il suo stderr (il log applicativo) finisce già nel journal come testo, con `SYSLOG_IDENTIFIER` uguale al nome del processo. Con la destinazione `journald` ogni record compare due volte: conviene ridurre il log applicativo (`logging.level: warn`) o mandare stderr altrove (`StandardError=null` nella unit). `journald` e `unixgram` non sono disponibili su Windows.

## Attributi utili

| Attributo | Dove compare |
|---|---|
| `deployment` | Tutti i log relativi a un deploy |
| `component` | `engine`, `worker`, `step`, `api`, `dbos`, `callback`, `plugin-process` |
| `plugin` | Log dello step `deploy` e dei plugin |
| `executor` | Log del worker |
| `dry_run` | Accodamento e verifiche in dry-run |
| `delivery` | Invio dei callback |

## Log dei plugin esterni

I plugin esterni scrivono i log su stderr in JSON compatibile con hclog. manent li inoltra al proprio logger con livello, messaggio e attributi originali. Vedi [Plugin esterni](../plugins/esterni.md#log).
