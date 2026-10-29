# Rilasci a ondate

Con più region e AZ un deploy si rilascia a **ondate**: una canary, poi una region alla volta, un'AZ alla volta, con un tempo di osservazione e un **health gate** tra un passo e il successivo. Il deploy resta uno solo (un ID, un esito) e si ferma, o torna indietro, al primo passo che non supera il controllo.

Dietro un load balancer ogni lotto può essere [svuotato](svuotamento.md) prima del deploy e rimesso in servizio dopo. Per spostare il traffico per percentuali verso una versione affiancata, invece che per nodi, si usa la strategia [`traffic`](traffico.md).

```mermaid
flowchart TB
    C[Config e piano] --> K[Canary: 1 nodo in eu-1a]
    K --> G1{Gate verde?}
    G1 -->|sì · approvazione, se richiesta| E1[eu-1, un'AZ alla volta]
    E1 --> G2{Gate verde?}
    G2 -->|sì| R[eu-2 e us-1 insieme, un'AZ alla volta]
    R --> G3{Gate verde?}
    G3 -->|sì| OK[Deploy riuscito]
    G1 -->|no| F[onFailure: stop, rollback-wave, rollback-all]
    G2 -->|no| F
    G3 -->|no| F
```

## Topologia: le etichette dei target

Ogni target porta delle etichette (`labels`); `region` e `az` sono riservate. L'ambiente resta uno (`prod`): le region non diventano ambienti separati, così governance, inventario e promozione ragionano su `prod`.

```yaml title="Configurazione di ambiente myapp/prod"
plugin: remote-copy
targets:
  - {host: eu1-a-01, labels: {region: eu-1, az: eu-1a, canary: "true"}}
  - {host: eu1-a-02, labels: {region: eu-1, az: eu-1a}}
  - {host: eu1-b-01, labels: {region: eu-1, az: eu-1b}}
  # ... 3 region × 3 AZ
```

## La strategia

La strategia (`rollout`) sta di norma nella [configurazione di ambiente](configurazione-ambiente.md), autorevole: il descrittore può ripeterla identica, non cambiarla. Senza `rollout` il deploy resta quello di sempre, su tutti i target in un unico passo.

```yaml
rollout:
  regions: [eu-1, eu-2, us-1]          # ordine delle region (le altre seguono in ordine alfabetico)
  waves:
    - {name: canary, select: "canary=true", bake: 15m}
    - {name: eu-1, select: "region=eu-1, canary!=true", by: az, bake: 10m}
    - {name: resto, select: "region in (eu-2, us-1)", by: az, parallelRegions: 2, bake: 10m}
  maxUnavailable: 25%                  # target fuori servizio insieme in un passo
  onFailure: stop                      # stop | rollback-wave | rollback-all
  approvalsBetween: [canary]           # pausa per approvazione dopo queste ondate
  health:
    interval: 30s
    failuresToStop: 2
    probes:
      - {url: "https://{host}/healthz", expectStatus: 200}
    metrics:
      - name: error-rate
        query: 'sum(rate(http_requests_total{app="{application}",code=~"5..",az="{az}"}[5m])) / sum(rate(http_requests_total{app="{application}",az="{az}"}[5m]))'
        max: 0.01
    alerts:
      blockOn: ["severity=critical"]
```

| Campo | Default | Significato |
|---|---|---|
| `waves[].select` | | Target dell'ondata: condizioni separate da virgole, tutte necessarie (`chiave=valore`, `chiave!=valore`, `chiave in (a, b)`, `chiave notin (a, b)`). |
| `waves[].by` | nessuna divisione | `az`: un passo per AZ; `region`: un passo per region. |
| `waves[].parallelRegions` | `1` | Region aggiornate insieme nello stesso stadio. Con `by: az`, lo stadio k contiene l'AZ k di ciascuna region (in ordine alfabetico). |
| `waves[].bake` | `0s` | Tempo di osservazione dopo ogni stadio dell'ondata (al massimo 24h). |
| `waves[].batchBake` | nessun gate | Se presente, gate dopo ogni lotto di un passo tranne l'ultimo: le sonde leggono i target del lotto per questo tempo (`0s`: una sola lettura). Vedi [gate per lotto](#gate-per-lotto). |
| `waves[].maxUnavailable` | quello del rollout | Sostituisce `maxUnavailable` per i passi dell'ondata: ad esempio un nodo alla volta nel canary e il 25% nelle ondate ampie. |
| `waves[].parallel` | `deploy.parallel` | Sostituisce `deploy.parallel` per i lotti dell'ondata. Vedi [target in parallelo](#target-in-parallelo). |
| `maxUnavailable` | `1` | Target di un passo fuori servizio insieme (un lotto): un numero o una percentuale dei target del passo. |
| `onFailure` | `stop` | Cosa fare se un passo o un gate fallisce (vedi sotto). |
| `approvalsBetween` | | Ondate dopo le quali il rollout attende un'approvazione. |
| `health` | | Controlli del gate. Senza, il gate è solo il tempo di osservazione. |

Il piano viene verificato all'accodamento (`422` se non valido): ogni ondata deve selezionare almeno un target, ogni target deve appartenere a una sola ondata, con `by` i target devono avere le etichette `region` e `az`. `manent validate` mostra gli stadi e i passi:

```sh
manent validate -f release.yml     # rollout.stages: canary, eu-1/eu-1b, resto/eu-2a+us-1a, ...
```

### Stadi, passi e lotti

- Uno **stadio** è un gruppo di passi eseguiti insieme e seguiti dallo stesso bake e health gate. Il suo nome è `<ondata>/<AZ o region>` (`resto/eu-2a+us-1a`).
- Un **passo** è il lavoro su una region (o un'AZ) dentro lo stadio: `<ondata>/<region>/<az>`.
- Dentro un passo i target sono aggiornati a **lotti** di `maxUnavailable` (quello dell'ondata, se presente, altrimenti quello del rollout), un lotto dopo l'altro. Una percentuale si calcola sui target del passo, arrotondando per difetto e con almeno un target.

Gli step che lavorano sui target, da `deploy` ad `activation` (con `post-test` in mezzo), sono eseguiti per ogni lotto; quelli prima (`secrets`, `artifacts`, `qa`) una volta sola all'inizio. Nel log e nella timeline ogni esecuzione porta il nome del passo e del lotto: `deploy@eu-1/eu-1/eu-1b#0`.

### Target in parallelo { #target-in-parallelo }

`maxUnavailable` dice quanti target sono fuori servizio insieme; `parallel` dice quanti di questi il plugin aggiorna **nello stesso momento**. Di default (`1`) il plugin tratta i target di un lotto uno dopo l'altro: con 20 host in un lotto e una copia di 30 secondi, il lotto dura 10 minuti. Con `parallel: 10` dura circa un minuto, e lotti, gate e approvazioni restano esattamente come prima.

```yaml
deploy:
  parallel: 10            # configurazione di ambiente: parallel: 10
rollout:
  maxUnavailable: 50%     # 40 host: due lotti da 20, uno dopo l'altro, con il gate in mezzo
  waves:
    - {name: canary, select: "tier=canary", parallel: 1}   # un host alla volta
    - {name: all, select: "tier=web"}                      # 10 host alla volta
```

- **Valore.** Un numero o una percentuale del lotto (`25%`), almeno 1. `waves[].parallel` sostituisce `deploy.parallel` per i lotti di quell'ondata. Senza rollout c'è un solo lotto con tutti i target, quindi `deploy.parallel` funziona anche da solo.
- **Limiti.** Il parallelismo effettivo di un lotto è il minimo tra quanto richiesto, `plugins.<nome>.max_parallel` in `manent.yml` (default 16, scelto dalla piattaforma per proteggere worker e destinazioni) e la dimensione del lotto. `descriptor_policy.max_parallel` limita quanto può chiedere un descrittore (`422` oltre il limite); un valore nella configurazione di ambiente non vi è soggetto.
- **Plugin.** Aggiornano i target in parallelo solo i plugin che lo dichiarano: tra quelli inclusi `remote-copy`, `container`, `jboss`, `windows`, `kubernetes` ed `ecs`, e i [plugin esterni](../plugins/esterni.md#parallel-targets) con la capability `parallel_targets`. Con gli altri plugin i target restano in sequenza, con un warning nel log del deploy.
- **Errori.** Dopo il primo target fallito nessun altro target del lotto parte, e quelli già avviati finiscono (interrompere una copia a metà lascerebbe uno stato più difficile da recuperare). I target mai avviati risultano `skipped`. Un rollback ripristina i target del lotto con lo stesso parallelismo.
- **Durabilità.** Il lotto resta un solo step DBOS: retry e recovery ripetono l'intero lotto, come prima, e il `Deploy` del plugin deve essere idempotente. Step e nomi non cambiano, quindi i deploy già in corso quando manent viene aggiornato proseguono invariati.
- **Dove vederlo.** L'output dello step `deploy` riporta `parallel` (effettivo) e, quando è stato ridotto, `parallel_requested`; `manent deploy` su terminale lo mostra accanto allo step (`deploy@all#0  42s · 10 at a time`). `manent_target_deploy_seconds` misura il tempo speso su ogni target.

`parallel` non è ammesso con la strategy `traffic`: lo slot candidato non ha lotti.

## Health gate

Dopo ogni stadio il worker attende il tempo di osservazione leggendo i controlli a ogni `interval`. Lo stadio fallisce appena i controlli rossi consecutivi arrivano a `failuresToStop` (default 1), e alla fine del bake l'ultima lettura deve essere verde. Attese e letture sono step durevoli: dopo una recovery il bake già trascorso non viene ripetuto.

| Controllo | Fonte | Rosso quando |
|---|---|---|
| `probes` | HTTP verso ogni target dello stadio (`{host}` sostituito) | Stato diverso da `expectStatus` (default 200), o errore. L'URL deve iniziare con `http(s)://{host}`: le sonde raggiungono solo i target. |
| `metrics` | Query sul sistema di metriche di `provider` (default Prometheus, `rollout.prometheus_url`; anche Datadog, New Relic, Elastic, CloudWatch, Azure Monitor: vedi [analisi canary](traffico.md#analisi-canary)), per ogni passo; `{application}`, `{environment}`, `{region}` e `{az}` sostituiti | Valore oltre `max` o sotto `min`, oppure **nessun dato** (un gate che non vede traffico non può dire che il rilascio è sano: per accettarlo, `or vector(0)`). |
| `alerts` | Alertmanager (`rollout.alertmanager_url`), allarmi attivi | Un allarme soddisfa tutte le condizioni di `blockOn` e riguarda l'applicazione (`application` o `app`), la region (`region`) e l'AZ (`az`) dello stadio; un'etichetta assente nell'allarme vale per tutti. |

Lo stato del rollout riporta l'ultimo gate rosso: `stages[].gate`, ad esempio `metric error-rate (us-1/us-1a): valore 0.2 fuori soglia`.

### Gate per lotto { #gate-per-lotto }

Il gate dello stadio arriva dopo l'ultimo lotto: con molti target in un'AZ, un difetto che si vede subito viene rilevato solo quando l'intera AZ è aggiornata. Con `batchBake` il worker controlla ogni lotto prima di passare al successivo:

```yaml
rollout:
  waves:
    - {name: eu-1, select: "region=eu-1", by: az, bake: 10m, batchBake: 1m}
  maxUnavailable: 25%
  health:
    interval: 15s
    failuresToStop: 2
    probes:
      - url: "http://{host}:8080/healthz"
```

- Dopo ogni lotto, tranne l'ultimo (seguito dal gate dello stadio), le `probes` leggono **solo i target del lotto** ogni `interval` per `batchBake`, con le stesse regole del gate dello stadio: `failuresToStop` letture rosse consecutive fermano il passo, e l'ultima lettura deve essere verde. Con un pool il gate parte dopo che il lotto è tornato in servizio.
- `metrics` e `alerts` restano al gate dello stadio: descrivono l'intero passo e sono letti dall'orchestratore. Senza `probes`, `batchBake` è solo un'attesa durevole fra i lotti.
- Un gate per lotto rosso è un passo fallito: vale `onFailure`, e i lotti successivi non vengono toccati.
- Con `dbos.region_routing` il gate gira nel workflow figlio del passo, sul worker della region.
- Le letture sono step `gate@<passo>#<lotto>.<n>`. Non è ammesso con `strategy: traffic` e non è eseguito in dry-run.

## Fallimenti e rollback

Un passo fallito (uno step in errore o un gate rosso) ferma sempre gli stadi successivi. `onFailure` decide cosa succede a quelli già eseguiti:

| `onFailure` | Stadio fallito | Stadi già superati |
|---|---|---|
| `stop` (default) | Lasciato com'è | Restano sulla nuova versione: l'[inventario](governance.md#inventario) segnala `drift` |
| `rollback-wave` | Riportato alla release precedente | Restano sulla nuova versione |
| `rollback-all` | Riportato alla release precedente | Riportati indietro anche loro, in ordine inverso |

I rollback usano le stesse compensazioni del [rollback automatico](rollback.md), con i checkpoint presi all'inizio di ogni passo; per questo `rollback-wave` e `rollback-all` richiedono il rollback attivo (`rollback.enabled`) per il tipo di artefatto, e sono rifiutati all'accodamento altrimenti. Le migration restano escluse.

Dopo uno stop il deploy è `ERROR`: si corregge e si rilascia di nuovo la stessa versione (gli stadi già aggiornati vengono rieseguiti, quindi i plugin devono essere idempotenti) oppure si torna indietro con `manent rollback`.

## Approvazioni e freeze

- **Approvazione tra le ondate:** dopo l'ultimo stadio di un'ondata in `approvalsBetween` il rollout si mette in pausa e il deploy risulta `AWAITING_APPROVAL`. Si sblocca con `manent approve <id>`, con le stesse regole delle [approvazioni](governance.md#approvazioni): permesso `deployment.approve`, niente auto-approvazione, numero di approvazioni e scadenza dalla regola di governance (una approvazione e 72 ore se la regola non ne prevede). Un rifiuto o una scadenza fermano il rollout e applicano `onFailure`.
- **Freeze per region:** una finestra con `regions: [us-1]` blocca solo i passi su quella region. Prima di ogni stadio il worker verifica i freeze delle sue region e, se ce n'è uno attivo, mette il rollout in pausa (`paused-freeze`) fino alla fine della finestra. `freezeOverride` deroga anche a questi.

## Esecuzione nelle region

Di default l'orchestratore esegue tutti i passi. Con reti separate per region, i target di una region sono raggiungibili solo da worker di quella region: con `dbos.region_routing: true` ogni passo diventa un workflow figlio (`manent.deploy-pass`) sulla coda della sua region, eseguito da un worker con `dbos.region` uguale.

```yaml title="manent.yml del worker di eu-1"
dbos:
  region: eu-1
  region_routing: true
```

| Worker | Ascolta |
|---|---|
| con `region` | coda dei deploy, approvazioni e coda dei passi della sua region (`<queue>.<region>`) |
| senza `region`, con `region_routing` | coda dei deploy e approvazioni: orchestra, ma non esegue passi |
| senza `region_routing` | tutte le code (installazioni in una sola region) |

- Le region dello stesso stadio sono aggiornate in parallelo; ogni passo scarica e verifica l'artefatto sul worker della sua region.
- L'orchestratore attende i figli in modo durevole: se cade, chi lo riprende ritrova i passi già avviati e non li ripete. Un worker di region perso viene ripreso dalla [recovery](../operazioni/affidabilita.md) sulla stessa coda, quindi da un altro worker della stessa region.
- Una region senza worker registrati fa fallire il passo con `no worker registered for region <region>`.
- Anche le sonde HTTP dell'health gate (`probes`) sui target di una region sono eseguite da un worker di quella region, con un workflow figlio (`manent.gate-probe`) per ogni lettura; metriche e allarmi li legge l'orchestratore, che quindi deve raggiungere solo Prometheus e Alertmanager. Come per i passi, se la region non ha worker attivi la lettura attende il loro ritorno.
- I log scritti nei passi arrivano al log del deploy alla fine di ciascun passo.
- In multi-region tutti i worker devono avere `region_routing: true` (e di norma una `region`): un worker senza instradamento ascolterebbe anche le code delle region.

## Stato del rollout

`manent status <id>` (e `GET /api/v1/deployments/{id}`) riporta lo stato completo; gli elenchi e l'inventario il riassunto `rollout_progress` (stadi superati/stadi totali).

```json
{
  "state": "ERROR",
  "rollout_progress": "2/4",
  "rollout": {
    "status": "failed",
    "stage": "resto/eu-2a+us-1a",
    "on_failure": "stop",
    "stages": [
      {"name": "canary", "wave": "canary", "state": "succeeded", "passes": [{"name": "canary", "region": "eu-1", "az": "eu-1a", "hosts": ["eu1-a-01"]}]},
      {"name": "eu-1/eu-1b", "wave": "eu-1", "state": "succeeded", "passes": ["…"]},
      {"name": "resto/eu-2a+us-1a", "wave": "resto", "state": "failed",
       "gate": "metric error-rate (us-1/us-1a): valore 0.2 fuori soglia", "passes": ["…"]},
      {"name": "resto/eu-2b+us-1b", "wave": "resto", "state": "pending", "passes": ["…"]}
    ]
  }
}
```

| `rollout.status` | Significato |
|---|---|
| `running` | In esecuzione (uno stadio, il suo bake o il gate). |
| `paused-approval` | In attesa di approvazione dopo un'ondata. |
| `paused-freeze` | In attesa della fine di un freeze (`paused` dice quale). |
| `succeeded` / `failed` | Concluso; con un rollback, `rollback` ne riporta l'esito. |

La metrica `manent_rollout_passes_total{application,environment,region,az,outcome}` conta i passi conclusi (vedi [Osservabilità](../operazioni/osservabilita.md)).
