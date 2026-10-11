# Log e SIEM

Ogni componente di manent scrive su un unico logger `slog`, che invia ogni record a due destinazioni:

- il **log applicativo** su stderr, in formato `text` o `json` (`logging.format`);
- il **SIEM**, se `siem.enabled` è `true`: un messaggio RFC 5424 per record, via UDP oppure TCP (octet counting, RFC 6587).

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

Un SIEM irraggiungibile non blocca manent: l'errore di invio viene scritto sul log applicativo e la connessione viene riaperta all'invio successivo.

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
