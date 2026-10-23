# jboss

Il plugin `jboss` rilascia applicazioni Java EE / Jakarta EE (`war`, `ear`) su JBoss EAP e WildFly tramite l'**API HTTP di management**, su server standalone o su un domain controller. Non servono SSH né `jboss-cli` sul worker: carica l'archivio nel repository dei contenuti e cambia versione con operazioni di management.

Versioni supportate: JBoss EAP 7.4 e successive, WildFly 23 e successive (parametro `suspend-timeout` di `suspend`).

## Come cambia la versione

Ogni versione diventa un deployment distinto, `<artefatto>-<versione>.<ext>` (ad esempio `shop-1.4.0.war`), con lo stesso **runtime-name** (default `<artefatto>.<ext>`, così il context root non cambia). Per un'applicazione con un solo artefatto il nome dell'artefatto è quello dell'applicazione, quindi i nomi restano quelli dei rilasci precedenti; due artefatti di un'applicazione sono due applicazioni sul server. Per le applicazioni di un [tenant](../guida/tenant.md) entrambi i nomi iniziano con il tenant (`payments.shop-1.4.0.war`, runtime-name `payments.shop.war`): due applicazioni su un server non possono condividere un context root. Per ogni target, in sequenza:

1. legge i deployment; se la versione richiesta è già attiva con lo stesso contenuto non tocca nulla, quindi retry e recovery non riavviano l'applicazione;
2. carica l'archivio (`/management/add-content`) e aggiunge il deployment, **disattivato**;
3. con `suspend: true` sospende il server: le nuove richieste ricevono `503` e quelle in corso hanno `suspend_timeout` per terminare;
4. cambia versione con `replace-deployment` (in domain mode per ogni server group) dentro una sola operazione **composite**: se la nuova versione non parte, il server annulla l'intera operazione e la versione precedente resta attiva;
5. con `restart: true` riavvia la JVM, con `reload: true` ricarica il server, e in entrambi i casi attende che torni `running`. Altrimenti, se l'aveva sospeso, lo rimette in servizio;
6. rimuove le versioni più vecchie oltre `keep_releases`, mai quella attiva né la precedente.

Se il cambio fallisce dopo una sospensione, il plugin rimette in servizio il server prima di restituire l'errore, così la versione precedente torna a ricevere richieste.

!!! note "Stessa versione, contenuto diverso"
    Se è già attivo un deployment con lo stesso nome ma contenuto diverso (un artefatto ricostruito con la stessa versione), il deploy fallisce: serve una nuova versione. Un deployment disattivato con contenuto diverso viene sostituito.

## Configurazione

```yaml title="manent.yml"
plugins:
  jboss:
    artifact_types: [war, ear]          # default
    ca_file: /etc/manent/jboss-ca.pem   # CA del certificato di management, se privata
    insecure_skip_tls_verify: false     # true solo in sviluppo
    request_timeout: 10m                # una chiamata, caricamento compreso
```

## Parametri del deploy

| Parametro | Default | Descrizione |
|---|---|---|
| `username` | | Utente di management (`add-user.sh`, `ManagementRealm`). |
| `password_secret` | | Nome del segreto con la password di management. |
| `tls` | `true` | `https` verso l'interfaccia di management (porta `9993`); `false` per `http` (porta `9990`). La `port` del target sostituisce la porta. |
| `server_groups` | | Solo in domain mode: server group su cui rilasciare, separati da virgola. |
| `runtime_name` | `<artefatto>.<ext>` (`<tenant>.<artefatto>.<ext>` per un tenant) | Nome di runtime comune a tutte le versioni (ad esempio `ROOT.war`). |
| `suspend` | `false` | Sospende il server (in domain mode i server group) prima del cambio. |
| `suspend_timeout` | `60s` | Tempo concesso alle richieste in corso durante la sospensione. |
| `reload` | `false` | Ricarica il server dopo il cambio, senza riavviare la JVM. |
| `restart` | `false` | Riavvia la JVM dopo il cambio. Alternativo a `reload`. |
| `keep_releases` | `3` | Versioni conservate nel repository dei contenuti, compresa l'attiva (almeno 2, per conservare la precedente per il rollback). |
| `wait_timeout` | `5m` | Attesa del server dopo `reload` o `restart`. |

I parametri booleani accettano `true` e `false`. L'autenticazione segue la sfida del server: digest (MD5 o SHA-256, il default del realm di management) o basic. Il plugin non segue mai i redirect, quindi le credenziali vanno solo all'host del target.

`artifact.version` è obbligatoria e diventa parte del nome del deployment. L'estensione viene dal file scaricato o da `artifact.type` (`.war`, `.ear`, `.jar`, `.rar`, `.sar`).

```yaml title="envconfig/shop/shop/prod.yml"
plugin: jboss
targets:
  - {host: eap1.example.com, labels: {region: eu-1, az: eu-1a}}
  - {host: eap2.example.com, labels: {region: eu-1, az: eu-1b}}
params:
  username: manent
  password_secret: jboss-mgmt
  suspend: "true"
  suspend_timeout: 90s
secrets:
  - {name: jboss-mgmt, ref: "vault:secret/data/shop/prod#jboss_password"}
rollback:
  enabled: true
```

Nella [policy sui descrittori](../guida/configurazione-ambiente.md) ammetti solo i parametri che un team può cambiare, ad esempio `jboss: [suspend, suspend_timeout, reload, restart, wait_timeout]`: utente, segreti e server group appartengono alla configurazione di ambiente.

### Standalone e domain mode

Il plugin legge `launch-type` dal target:

- **standalone:** ogni target è un server. I target sono rilasciati uno alla volta; con un [rilascio a ondate](../guida/ondate.md) con `maxUnavailable` e un [pool del load balancer](../guida/svuotamento.md) che svuota ogni lotto, gli utenti non vedono mai un server durante il cambio.
- **domain:** il target è il **domain controller** e `server_groups` è obbligatorio. Il deployment viene aggiunto al repository dei contenuti del dominio e assegnato ai gruppi; sospensione, ripresa, reload e restart valgono per tutti i server dei gruppi (`suspend-servers`, `reload-servers`, `restart-servers` con `blocking`). Un target per dominio.

### `restart` in standalone

In standalone il restart è `shutdown(restart=true)`: il server termina con il codice che chiede a `standalone.sh` di riavviarlo. Funziona se il server è avviato da `standalone.sh` (direttamente o dal servizio systemd della distribuzione); un server avviato in altro modo non torna, e il deploy fallisce dopo `wait_timeout`.

## Rollback

- **Release in esercizio** (locator `plugin`): il deployment attivo con il runtime-name dell'applicazione, sul primo target. Un deployment non creato da manent (rilasciato a mano) viene riconosciuto per nome, quindi può comunque essere la destinazione di un rollback.
- **Rollback:** su ogni target riattiva il deployment precedente, ancora presente nel repository dei contenuti, con la stessa operazione composite e gli stessi `suspend`, `reload` e `restart`. Non carica nulla, quindi non dipende dalla sorgente dell'artefatto.

I tipi di artefatto `war` ed `ear` ammettono il rollback (`rollback.artifact_types.war.allowed: true` nei default).

## Dry-run

In [dry-run](../guida/dry-run.md), per ogni target: autenticazione, modalità, server group, `server-state` (`running` in standalone) e versione attiva. Non modifica nulla.

## Risultato

```json
{
  "plugin": "jboss",
  "targets": [
    {
      "host": "eap1.example.com",
      "status": "deployed",
      "details": {
        "mode": "standalone",
        "deployment": "shop-1.4.0.war",
        "runtime_name": "shop.war",
        "previous": "shop-1.3.0.war",
        "previous_version": "1.3.0",
        "suspended": "true"
      }
    }
  ]
}
```
