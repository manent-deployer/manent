# Policy in Rego

Con `governance.engine: opa` le [regole di governance](governance.md) si scrivono in **Rego** e le valuta OPA incorporato in manent: niente sidecar, nessuna chiamata di rete. La policy si può cambiare **a runtime**. Una nuova revisione viene caricata, testata, misurata sui deploy reali e approvata da un'altra persona, e ogni processo la riprende in pochi secondi, senza riavvii.

Le regole di `manent.yml` continuano a funzionare: senza una revisione, la policy predefinita le applica invariate (`governance.rules` diventano i dati `data.manent.rules`).

```yaml title="manent.yml"
governance:
  engine: opa                 # builtin (default) | opa
  refresh: 10s                # ogni quanto ogni processo rilegge policy e freeze a runtime
  max_staleness: 10m          # oltre questo senza una lettura riuscita, i deploy reali sono rifiutati
  start: deny                 # deny | strict: cosa ricontrolla l'avvio del deploy (vedi sotto)
  opa:
    source: store             # store (default) | file | bundle | consul
    store:
      review: true            # approvazione da un'identità diversa dall'autore (quattro occhi)
      tests: run              # run (default) | required: revisioni senza test rifiutate
    # query: data.manent.governance.decision
    # eval_timeout: 200ms
  rules: [...]                # dati della policy predefinita
  freezes: [...]
```

## La decisione

La policy riceve un **input** che descrive il deploy e restituisce una **decisione**: divieti e obblighi. manent fa rispettare gli obblighi come con le regole YAML. La policy dice *cosa* serve e manent lo verifica: lo stesso artefatto già rilasciato negli ambienti di `requires`, le approvazioni, il ticket di change.

### Input

```json
{
  "schema": 3,
  "phase": "submit",
  "now": "2026-10-18T09:30:00Z",
  "application": "shop",
  "environment": "prod",
  "region": "",
  "deployment": {"id": "ci-42", "kind": "deploy", "source": "", "dry_run": false, "schedule": {},
                 "freeze_override": "", "after": [], "release": ""},
  "artifact": {"name": "shop", "version": "1.4.0", "type": "application", "sha256": "…", "source": "oci://…"},
  "requested_by": {"id": "oidc:alice", "method": "oidc", "name": "alice", "groups": ["team-shop"]},
  "plugin": "kubernetes",
  "rollout": {"strategy": "waves", "waves": 3, "regions": ["eu", "us"], "approval_points": 1},
  "targets": [{"host": "k8s-eu", "labels": {"region": "eu"}}],
  "current": {"id": "…", "version": "1.3.2", "sha256": "…", "deployed_at": "2026-10-10T12:00:00Z"},
  "freezes": {"active": [{"name": "year-end", "end": "2027-01-07T00:00:00+01:00", "hard": false}]}
}
```

| Campo | Note |
|---|---|
| `phase` | `submit` (accodamento), `validate`, `start` (avvio del deploy sul worker), `stage` (inizio di uno stadio del rollout, una volta per region), `tick` (esecuzione di una schedule ricorrente). |
| `now` | L'istante della valutazione: l'accodamento (o `schedule.at`), oppure il tempo letto dallo step che valuta. Le policy usano questo campo, perché l'orologio non è disponibile. |
| `deployment.kind` | `deploy`, `promotion`, `rollback`. |
| `targets`, `plugin`, `rollout` | Dal deploy effettivo (configurazione di ambiente fusa), quando è noto. |
| `current` | Release in esercizio nell'ambiente; assente al primo rilascio. |
| `freezes.active` | Finestre attive a `now`, calcolate da manent. |
| `tenant`, `application_tags` | Il [tenant](tenant.md) della release (`name`, `tags`; `untenanted` per le applicazioni che nessun tenant elenca, anche senza registro) e i tag dell'applicazione nel registro. Schema 3 (lo schema 2 aveva il tenant come cliente e `tenants_run`). |

L'input non contiene segreti né parametri dei plugin. Lo schema è versionato (`schema`): i campi si aggiungono e non vengono mai rinominati.

### Decisione

La query `data.manent.governance.decision` deve produrre sempre un oggetto (si dichiara un `default`): una decisione indefinita rifiuta il deploy.

| Chiave | Descrizione |
|---|---|
| `rule` | Nome descrittivo, mostrato nello stato e da `validate`. |
| `deny` | Insieme di `{"code": …, "message": …}` (o di stringhe): un insieme non vuoto rifiuta il deploy con i messaggi. |
| `requires` | Ambienti in cui lo stesso artefatto deve essere già stato rilasciato. |
| `approvals` | `required`, `allow_self`, `timeout` (`"72h"`), `when` (`before`/`rollout`) e `approvers` (`groups`, `subjects`): chi può approvare, oltre al permesso `deployment.approve`. `null` o `required: 0`: nessuna approvazione. |
| `change` | `approval`, `timeout`: ticket di change nell'ITSM ([change management](integrazioni.md#change-management)). |
| `freeze_exempt` | Nomi delle finestre attive che la policy esenta. Una policy può solo esentare: le finestre attive bloccano per default, e quelle `hard` non sono mai esentate. |
| `notes` | Spiegazioni mostrate da `validate`, dalla CLI e dall'interfaccia web. |

Chiavi sconosciute, tipi errati e durate non valide sono errori.

### Esempio

```rego title="governance.rego"
package manent.governance

import rego.v1

import data.manent.lib

prod if lib.match_any(["prod", "prod-*"], input.environment)

requires contains "preprod" if {
	prod
	input.deployment.kind != "rollback" # a rollback must never wait for preprod
}

default approvals := null

approvals := {"required": 2, "timeout": "72h", "approvers": {"groups": ["dba"]}} if {
	prod
	input.artifact.type == "migration"
} else := {"required": 1} if prod

default change := null

# Out of business hours, the change approved in the ITSM too.
change := {"approval": true, "timeout": "24h"} if {
	prod
	not lib.business_hours(input.now, "Europe/Rome")
}

# Hotfixes with an incident pass the weekend freeze (never a hard one).
freeze_exempt contains "weekend" if {
	endswith(input.artifact.version, "-hotfix")
	startswith(input.deployment.freeze_override, "INC-")
}

deny contains {"code": "downgrade", "message": "downgrades in production go through manent rollback"} if {
	prod
	input.deployment.kind == "deploy"
	lib.semver_less(input.artifact.version, input.current.version)
}

decision := {
	"rule": "custom",
	"deny": deny,
	"requires": requires,
	"approvals": approvals,
	"change": change,
	"freeze_exempt": freeze_exempt,
}
```

### Libreria e restrizioni

`import data.manent.lib` mette a disposizione funzioni deterministiche:

| Funzione | Descrizione |
|---|---|
| `match_any(patterns, value)` | Pattern con `*` come in `manent.yml` (`glob.match` di OPA ha un'altra sintassi). |
| `semver_less(a, b)` | Versioni confrontate per numero; prefisso `v` ammesso, una pre-release precede la sua release. |
| `local_time(t, tz)` | `year`, `month`, `day`, `weekday` (0 = domenica), `hour`, `minute` di un istante in un fuso orario. |
| `business_hours(t, tz)` | Dalle 9:00 alle 18:00, dal lunedì al venerdì. |
| `tenant_matches(selettore, tenant)` | Un selettore di tenant su `input.tenant` (`{}` senza tenant: mai). |
| `app_match_any(pattern, input)` | Pattern qualificati delle applicazioni (`payments/*`, `*/*`; senza prefisso solo untenanted) sul tenant e l'applicazione dell'input, come le regole di `manent.yml`. |
| `app_matches(selettore, input)` | Un selettore sull'applicazione dell'input: i suoi tag, `application`, `tenant`, `tenant.<tag>`. |
| `duration_ns(d)` | Una durata (`"72h"`) in nanosecondi. |

`data.manent.lib.rules` implementa la semantica delle regole YAML su `data.manent.rules`: una policy può delegargli i casi che non gestisce (`decision := rules.decision if not mine`).

Una valutazione dipende solo da input e dati, quindi lo stesso input con la stessa revisione dà sempre la stessa decisione:

- non compilano i built-in che leggono l'orologio, la rete o il runtime (`time.now_ns`, `http.send`, `net.lookup_ip_addr`, `rand.intn`, `uuid.rfc4122`, `opa.runtime`, `trace` e gli altri non deterministici);
- Rego v1 in modalità strict;
- ogni valutazione dura al più `eval_timeout` (default 200ms);
- i package `manent.lib` sono riservati.

### Dati

Una revisione può contenere dati: `data.json` o `data.yaml` nella directory `a/b/` finiscono sotto `data.a.b`. Due percorsi sono speciali:

- `data.manent.rules`: regole con lo schema di `governance.rules` (durate come stringhe). Se la revisione non le definisce, si usano quelle di `manent.yml`.
- `data.manent.freezes`: finestre di freeze con lo schema di `governance.freezes` (più `hard`), per le sorgenti diverse dallo store (file, bundle, Consul), versionate insieme alla policy.

### Test

I moduli `*_test.rego` sono i test della revisione (`opa test`, con `with input as …`). Girano a ogni caricamento e un test fallito fa rifiutare la revisione. Con `store.tests: required` viene rifiutata anche una revisione senza test. Ogni revisione deve inoltre produrre una decisione valida su un insieme di input di esempio, uno per ogni fase e tipo di deploy.

```sh
manent-server policy test -d policies/                # compila e testa in locale, senza server
manent-server -c manent.yml policy eval -d policies/ -f deploy.yml [--phase start] [--kind rollback]
manent-server -c manent.yml policy export-rbac -d policies/   # binding di manent.yml come dati, rbac.engine: opa
```

Con `-c` le regole della configurazione diventano `data.manent.rules`, come sul server.

## Revisioni gestite da manent { #revisioni }

Con `source: store` (default) le revisioni stanno nel database di manent e si gestiscono con la CLI, l'API o l'[interfaccia web](interfaccia-web.md) (pagina Policy).

```sh
manent policy push -d policies/ -m "approvazione dei DBA per le migrazioni"   # bozza: compilata e testata dal server
manent policy diff 12 13                                                    # differenze tra due revisioni
manent policy impact 13 --since 30d                                         # decisioni dei deploy reali ricalcolate
manent policy eval 13 -f deploy.yml                                         # decisione su un descrittore
manent policy approve 13                                                    # attivazione, da un'altra identità con review: true
manent policy activate 12                                                   # ritorno a una revisione già attiva in passato
manent policy list | show 13 | pull 13 -d out/
```

- **Prima bozza, poi attiva.** Una revisione caricata è una bozza e vale solo dopo l'approvazione. Con `store.review: true` l'autore non può approvarla (`403`).
- **Impatto.** `impact` ricostruisce gli input dei deploy del periodo dalle richieste registrate (con `now` uguale al loro accodamento) e confronta gli obblighi registrati allora con quelli della revisione. Così si vede prima dell'attivazione una revisione che bloccherebbe tutto o toglierebbe approvazioni.
- **Immutabilità.** Una revisione non cambia mai: una correzione è una nuova revisione. La identifica lo sha256 dei suoi file.
- **Propagazione.** Ogni processo rilegge la revisione attiva ogni `refresh`; su Postgres una notifica (`LISTEN/NOTIFY`) la porta in meno di un secondo. Una revisione non valida non sostituisce mai quella in vigore.
- **Ritorno.** `activate` riporta a qualsiasi revisione già stata attiva. Per tornare alle regole YAML si carica una revisione con il solo `data.json` contenente `{}`.

Senza alcuna revisione attiva, la policy predefinita applica le regole di `manent.yml`.

## Altre sorgenti

| Sorgente | Configurazione | Quando si rilegge |
|---|---|---|
| `file` | `opa.file.dir`: file *.rego e di dati, file nascosti esclusi (ad esempio una ConfigMap di Kubernetes) | cambiano nomi, dimensioni o date di modifica |
| `bundle` | `opa.bundle.url` (bundle OPA, tar.gz), `ca_file`, `credentials_ref` (bearer token), `signing.public_key_ref`, `algorithm` (default RS256), `key_id`, `scope` | cambia l'ETag (`If-None-Match`) |
| `consul` | `opa.consul.prefix` (default `manent/policies`): ogni chiave sotto il prefisso è un file; indirizzo e token di `envconfig.consul` | cambia l'indice del prefisso |

La **firma** del bundle (`.signatures.json`, come la produce `opa build --signing-key`) è obbligatoria. Un bundle non firmato è accettato solo con `insecure_skip_verify: true`, per lo sviluppo. Con queste sorgenti le revisioni sono versionate fuori da manent (Git, CI, `opa test`): `manent policy push` e `approve` non sono disponibili, e `policy list` mostra la revisione in vigore.

## Freeze a runtime { #freeze-a-runtime }

Le finestre di freeze si possono anche creare e chiudere a runtime, con entrambi i motori, senza modificare `manent.yml`:

```sh
manent freeze add --name incidente-881 --reason "incidente 881" -e prod [-a 'shop-*'] [--region us-1] --for 4h [--hard]
manent freeze add --name chiusura-anno -e prod --from 2026-12-31T18:00:00+01:00 --until 2027-01-02T08:00:00+01:00
manent freeze list
manent freeze delete incidente-881
```

- **Effetto.** Un deploy reale nella finestra viene rifiutato subito, e uno già in coda fallisce all'avvio. Un rollout a ondate in corso si ferma prima dello stadio successivo e riprende quando la finestra si chiude.
- **Hard.** Con `--hard` la policy non può esentare la finestra e `freezeOverride` non la supera.
- **Permesso.** Serve `freeze.write` su ogni pattern di applicazione e ambiente della finestra: un pattern `*` richiede il permesso su `*`. Lo concede il ruolo predefinito `release-manager`.
- **Elenco.** `manent freezes` e la pagina Calendario le mostrano con il loro autore (`dynamic`, `created_by`). Si possono eliminare solo le finestre create a runtime.

## Determinismo e registrazione

Una decisione si prende una volta, in un punto registrato, e poi si riusa:

| Fase | Dove | Effetto | Registrata in |
|---|---|---|---|
| `submit` | accodamento (deploy, promozione, rollback, ogni deploy di una release) | divieti, freeze, `requires`, obblighi | la richiesta: revisione, regola, hash dell'input (`policy` nello stato) |
| `start` | step `config` | divieti e freeze arrivati dopo l'accodamento | output dello step (`policy_revision`) |
| `stage` | step `policy@<stadio>#<n>` prima di ogni stadio del rollout | freeze per region (pausa) o divieto (il rollout si ferma) | output dello step |
| `tick` | step `policy` di ogni esecuzione di una schedule ricorrente | gli obblighi di quell'esecuzione | la richiesta dell'esecuzione |

Una recovery rilegge le decisioni registrate e non rivaluta mai: una revisione cambiata nel frattempo non cambia la sequenza degli step. I workflow accodati prima dell'introduzione della decisione mantengono il comportamento precedente.

**Gli obblighi si fissano all'accodamento:** una revisione più severa non invalida un'approvazione già data. Con `governance.start: strict` l'avvio rifiuta anche un deploy per cui le approvazioni o il change management sono aumentati dopo l'accodamento: va accodato di nuovo.

## Indisponibilità

| Situazione | Effetto |
|---|---|
| Nessuna revisione caricabile all'avvio | `/readyz` risponde `503`; i deploy reali sono rifiutati con `503` e lo step `config` viene ritentato. Dry-run, letture e annullamenti funzionano. |
| Sorgente irraggiungibile, revisione già caricata | resta in vigore quella caricata; oltre `max_staleness` come sopra. |
| Errore di valutazione (errore a runtime, timeout, decisione non valida o indefinita) | rifiuto, mai via libera: `422` all'accodamento, retry negli step. |

## Permessi e audit

| Azione | Consente |
|---|---|
| `policy.read` | Elencare e leggere le revisioni. |
| `policy.write` | Caricare revisioni, valutarle e misurarne l'impatto. |
| `policy.approve` | Approvare le bozze e tornare a revisioni precedenti. |
| `freeze.write` | Creare ed eliminare freeze a runtime. |

Le azioni `policy.*` non dipendono da applicazione e ambiente, come `platform.read`; tra i ruoli predefiniti le concede solo `admin`. Arrivano sempre da `rbac.bindings` di `manent.yml`, anche quando gli altri permessi li decidono le revisioni ([Permessi come policy](permessi.md)): una revisione non può concedersi il potere di cambiare la policy.

Ogni operazione è registrata nell'[audit](../operazioni/accesso.md#audit) (`policy.push`, `policy.activate`, `freeze.create`, `freeze.delete`). Ogni decisione è nei log (e arriva al SIEM) con revisione e hash dell'input.

**Break-glass.** Se una revisione blocca tutto, o se durante un incidente manca un secondo approvatore, un operatore con accesso al database può agire fuori dai permessi. L'operazione resta registrata nell'audit.

```sh
manent-server admin policy-activate 12 --reason "INC-881: la revisione 13 blocca l'hotfix"
manent-server admin freeze-delete incidente-881 --reason "freeze chiuso dall'incident manager"
```

## Metriche

| Metrica | Descrizione |
|---|---|
| `manent_policy_revision_info{engine,source,revision}` | Revisione in vigore nel processo. |
| `manent_policy_age_seconds` | Secondi dall'ultima lettura riuscita di policy e freeze. |
| `manent_policy_load_errors_total` | Letture fallite (revisione rifiutata o sorgente irraggiungibile). |
| `manent_policy_evaluations_total{phase,outcome}` | Valutazioni per fase ed esito (`allowed`, `denied`, `frozen`, `error`, `unavailable`). |
| `manent_policy_eval_duration_seconds{phase}` | Durata delle valutazioni. |

Un allarme su `manent_policy_age_seconds` che si avvicina a `max_staleness` arriva prima dei rifiuti.

## API

| Metodo | Percorso | Descrizione |
|---|---|---|
| `GET` | `/api/v1/policies` | Revisioni e policy in vigore. |
| `POST` | `/api/v1/policies` | Carica una bozza (`{"files": {...}, "comment": ...}`). |
| `GET` | `/api/v1/policies/{revision}` | Una revisione con i suoi file (`current`: quella in vigore). |
| `POST` | `/api/v1/policies/{revision}/evaluate` | Decisione su un descrittore (`phase`, `kind`). |
| `POST` | `/api/v1/policies/{revision}/access` | Permessi di un'identità, con `rbac.engine: opa` ([simulazione](permessi.md#simulazione-e-impatto)). |
| `GET` | `/api/v1/policies/{revision}/impact?since=30d` | Decisioni dei deploy reali ricalcolate. |
| `POST` | `/api/v1/policies/{revision}/approve` | Approva una bozza. |
| `POST` | `/api/v1/policies/{revision}/activate` | Torna a una revisione precedente. |
| `POST` | `/api/v1/freezes` | Crea un freeze a runtime. |
| `DELETE` | `/api/v1/freezes/{name}` | Elimina un freeze creato a runtime. |
