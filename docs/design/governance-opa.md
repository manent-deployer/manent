# Design: governance con OPA (Rego) e policy modificabili a runtime

| | |
|---|---|
| Stato | Implementato (vedi §17 per le differenze rispetto alla proposta) |
| Data | 2026-10-18 |
| Ambito | `internal/governance`, `internal/engine` (govern, config, rollout, schedule), `internal/authz`, `internal/app`, API/CLI/UI |

## 1. Contesto

Oggi la governance è dichiarata in `manent.yml` (`governance.rules`, `governance.freezes`), validata da `governance.New` e caricata una sola volta da `app.NewGovernance`. Ogni cambio richiede il riavvio di API e worker.

Le regole sono un elenco ordinato: vale la prima regola che corrisponde. Corrispondono per glob su application/environment e producono:

- `requires`: ambienti in cui lo stesso artefatto deve già essere riuscito;
- `approvals`: `required`, `allow_self`, `timeout`, `when`;
- `change`: ticket ITSM ed eventuale attesa della sua approvazione.

Ci sono poi le finestre di freeze, assolute o cron, eventualmente limitate a delle region.

Il modello ha due limiti:

1. **Espressività.** Una regola dipende solo da application e environment. Non si può dire, per esempio:
    - "le migrazioni in prod richiedono 2 approvazioni del gruppo DBA";
    - "durante il freeze passano solo le versioni `-hotfix` con un ticket di incident";
    - "i rollback non richiedono la promozione";
    - "i deploy fuori orario richiedono un'approvazione in più";
    - "un artefatto da un registry esterno richiede il change".
2. **Ciclo di vita.** Un freeze d'emergenza, una regola nuova o un'eccezione temporanea richiedono di modificare `manent.yml` su tutti i nodi e riavviarli. Il riavvio dei worker passa anche dal drain, quindi può durare quanto il deploy più lungo in corso.

## 2. Obiettivi e non obiettivi

**Obiettivi**

- Esprimere le regole di governance in **Rego** e valutarle con **OPA incorporato** nel processo (`github.com/open-policy-agent/opa/v1/rego`), senza sidecar.
- Rendere policy e finestre di freeze **modificabili a runtime**, senza riavvio, da sorgenti diverse:
    - store di manent con API/CLI/UI e revisione a quattro occhi;
    - directory locale;
    - bundle OPA firmati;
    - Consul.
- Mantenere le garanzie attuali:
    - decisioni deterministiche nei workflow DBOS (recovery e replay);
    - snapshot della decisione nella richiesta;
    - errori permanenti senza retry;
    - audit;
    - nessun segreto negli input.
- **Compatibilità totale:** senza configurazione nuova il comportamento non cambia. Le regole YAML esistenti restano valide e diventano un caso particolare del nuovo modello.

**Non obiettivi, per ora**

- Spostare in Rego l'RBAC (`rbac.bindings`) e la `descriptor_policy`. Il design lo rende possibile (§13), ma non fa parte delle prime fasi.
- Usare OPA come servizio remoto (`/v1/data`) per la decisione. Vedi le alternative (§15).
- Permettere alle policy di chiamare servizi esterni (`http.send`) durante la valutazione.

## 3. Dove si valuta la governance oggi

| Momento | Codice | Cosa valuta | Persistenza |
|---|---|---|---|
| Accodamento (`Submit`, `Promote`, `Rollback`) | `Engine.govern` (`engine.go`) | freeze all'istante (o a `schedule.at`), `requires` con `findRelease`, `approvals`, `change` | `Request.Approval`, `Request.Change` (snapshot) |
| Validazione e piano | `Engine.describeGovernance` | regola, freeze | nessuna |
| Avvio del deploy (step `config`) | `Worker.resolveConfig` | solo il freeze | output dello step DBOS |
| Stadio del rollout | `Worker.waitFreeze` (`rollout.go`) | `FrozenIn` per region, con `t` dallo step `now` | **nessuna**: valutata nel codice del workflow |
| Approvazione | `Guard.Decide` | auto-approvazione (`Approval.AllowSelf` dallo snapshot) | snapshot |
| Tick di una schedule | `manent.scheduled-deploy` | riusa `Request.Approval` della schedule | contesto della schedule |
| Elenco dei freeze | `Engine.Freezes` → `Windows` | finestre attive e future | nessuna |

L'ultima riga del rollout è il punto critico. Oggi `waitFreeze` è deterministico solo perché la policy è immutabile per tutta la vita del processo e uguale su tutti i nodi. Con policy modificabili, una recovery potrebbe prendere un ramo diverso (attesa invece di proseguire). Con DBOS, nel workflow un ramo diverso vuol dire una sequenza di step diversa. **Ogni valutazione dentro un workflow deve diventare l'output di uno step.**

## 4. Proposta in sintesi

1. Un'unica interfaccia di decisione, `governance.Evaluator`. Riceve un **input** strutturato, che descrive cosa si sta facendo, chi lo fa, quando e in quale fase. Restituisce una **decisione**, cioè divieti e **obblighi** (`requires`, `approvals`, `change`, freeze). Il motore applica gli obblighi come fa oggi: la policy decide *cosa serve*, manent *verifica e fa rispettare*.
2. Due implementazioni:
    - `builtin`, il codice attuale adattato all'interfaccia, di default;
    - `opa`, query preparate su un bundle Rego + dati.

    Le regole YAML esistenti diventano **dati** per una libreria Rego inclusa (`manent.lib.rules`) che ne riproduce la semantica. Così chi non vuole scrivere Rego può comunque modificare le regole a runtime come dati.
3. Le policy arrivano da una **sorgente** (`store`, `file`, `bundle`, `consul`) come **revisioni immutabili** identificate da un hash. Ogni processo ricarica la revisione attiva a intervalli. Una revisione non valida non sostituisce mai quella in uso.
4. Ogni decisione è **registrata con la revisione** che l'ha prodotta:
    - nella richiesta, al submit;
    - nell'output degli step DBOS, all'avvio, per stadio del rollout e per tick.

    Le recovery rileggono la decisione registrata e non rivalutano mai.
5. Le finestre di freeze diventano dati modificabili a runtime (`manent freeze add` per un freeze d'emergenza) anche con il motore `builtin`.

## 5. Modello della decisione

### 5.1 Input

L'input è un documento JSON costruito da manent. Non contiene segreti e usa solo dati già persistiti o persistibili. Il tempo è **sempre** un campo dell'input (`now`), mai `time.now_ns()`.

```json
{
  "phase": "submit",
  "now": "2026-10-18T09:30:00Z",
  "application": "shop",
  "environment": "prod",
  "region": "",
  "deployment": {
    "id": "ci-42",
    "kind": "deploy",
    "source": "",
    "dry_run": false,
    "schedule": {"at": "", "cron": ""},
    "freeze_override": "",
    "after": ["db-17"]
  },
  "artifact": {"name": "shop", "version": "1.4.0", "type": "application", "sha256": "…", "source": "oci://registry/shop@sha256:…"},
  "requested_by": {"id": "oidc:alice", "method": "oidc", "name": "alice", "groups": ["team-shop"]},
  "plugin": "kubernetes",
  "rollout": {"strategy": "waves", "regions": ["eu", "us"], "approval_points": 1},
  "targets": [{"host": "k8s-eu", "labels": {"region": "eu"}}],
  "current": {"version": "1.3.2", "sha256": "…", "deployed_at": "2026-10-10T12:00:00Z"},
  "freezes": {"active": [{"name": "year-end", "reason": "…", "end": "2027-01-07T00:00:00+01:00", "regions": []}]}
}
```

| Campo | Note |
|---|---|
| `phase` | `submit`, `validate`, `start`, `stage`, `tick` (§7). |
| `now` | Al submit è l'istante della richiesta, o `schedule.at`. Nelle altre fasi è il tempo letto dallo step DBOS che valuta. |
| `deployment.kind` | `deploy`, `promotion`, `rollback`: permette regole come "il rollback non richiede la promozione". |
| `targets`, `plugin`, `rollout` | Dal descrittore effettivo quando disponibile (`submit` con resolver, `start`); assenti se la configurazione di ambiente non è raggiungibile al submit. |
| `current` | Release in esercizio dall'inventario (`InventoryEntry`). Permette regole su "salto di major" o "downgrade". |
| `freezes.active` | Finestre attive calcolate da manent dai dati (§6): Rego non deve interpretare espressioni cron. |
| `region` | Valorizzato solo nella fase `stage`, una valutazione per region. |

Lo schema dell'input è **versionato** (`input.schema: 1`) e documentato. I campi si aggiungono in modo compatibile e non vengono mai rinominati.

### 5.2 Decisione

La query di default è `data.manent.governance.decision`. Il risultato deve rispettare lo schema seguente, che manent valida in Go: chiavi sconosciute, tipi errati, durate non valide e `required < 0` sono errori.

```json
{
  "rule": "production",
  "deny": [{"code": "major-upgrade", "message": "a major upgrade requires a release ticket"}],
  "requires": ["preprod"],
  "approvals": {
    "required": 2,
    "allow_self": false,
    "timeout": "72h",
    "when": "before",
    "approvers": {"groups": ["dba"], "subjects": []}
  },
  "change": {"approval": true, "timeout": "48h"},
  "freeze_exempt": ["weekend"],
  "notes": ["database migration: DBA approval required"]
}
```

- **`deny`**: insieme di divieti con codice e messaggio. Il messaggio è in inglese, come gli altri errori. Un `deny` non vuoto rifiuta il deploy con `ErrInvalidRequest`, e i messaggi finiscono nell'errore e nella validazione.
- **`requires`, `approvals`, `change`**: stessi significati e stessi tipi di oggi (`governance.Approvals`, `governance.Change`). Il nuovo campo è `approvals.approvers`: chi può approvare, oltre al permesso `deployment.approve`. Lo verifica `Guard.Decide` sullo snapshot.
- **Freeze**: le finestre attive bloccano **per default**. La policy può soltanto esentare una finestra per nome con `freeze_exempt`. Una policy vuota o sbagliata quindi non apre mai un freeze. Le finestre con `hard: true` non sono esentabili né dalla policy né con `freezeOverride`.
- **`notes`**: spiegazioni mostrate in `validate`, nella CLI e nella UI.
- **`rule`**: nome descrittivo, come `Rule.Name` oggi.

**Decisione indefinita = errore**, mai "nessun vincolo". Un package sbagliato o una regola che non produce `decision` rifiutano i deploy reali (§9.3). La libreria fornisce `default decision := {}` solo se la policy lo dichiara esplicitamente.

### 5.3 Esempio di policy

```rego
package manent.governance

import rego.v1

import data.manent.lib

# Production environments: prod, prod-eu, prod-us...
prod if lib.glob(["prod", "prod-*"], input.environment)

migration if input.artifact.type == "migration"

hotfix if endswith(input.artifact.version, "-hotfix")

requires contains "preprod" if {
	prod
	input.deployment.kind != "rollback" # rolling back must never wait for preprod
}

approvals := {"required": 2, "timeout": "72h", "approvers": {"groups": ["dba"]}} if {
	prod
	migration
} else := {"required": 1, "timeout": "72h"} if prod

# Out-of-hours deploys need the change approval in the ITSM.
change := {"approval": true, "timeout": "24h"} if {
	prod
	not lib.business_hours(input.now, "Europe/Rome")
}

# Hotfixes with an incident ticket pass the recurring weekend freeze, not year-end.
freeze_exempt contains "weekend" if {
	hotfix
	startswith(input.deployment.freeze_override, "INC-")
}

deny contains {"code": "downgrade", "message": "downgrades in production go through a manual rollback"} if {
	prod
	input.deployment.kind == "deploy"
	lib.semver_lt(input.artifact.version, input.current.version)
}

decision := {
	"rule": "custom",
	"deny": deny,
	"requires": requires,
	"approvals": object.get({"a": approvals}, "a", {"required": 0}),
	"change": object.get({"c": change}, "c", null),
	"freeze_exempt": freeze_exempt,
}
```

La libreria `data.manent.lib` fa parte di manent (non della revisione) e offre funzioni deterministiche registrate come built-in Go:

- `lib.glob`: stessa semantica di `internal/glob`, che non coincide con `glob.match` di OPA;
- `lib.business_hours`;
- `lib.semver_lt`;
- `lib.duration`.

### 5.4 Le regole YAML come dati

La libreria `manent.lib.rules` implementa in Rego la semantica attuale:

- vince la prima regola con application ed environment che corrispondono;
- se nessuna corrisponde non c'è nessun vincolo;
- timeout di 72h se `required > 0`.

Lavora su `data.manent.rules`, che ha lo stesso schema di `governance.rules` in YAML. La policy minima è:

```rego
package manent.governance

import data.manent.lib.rules

decision := rules.decision
```

Con il motore `opa` e senza una policy scritta dall'utente, manent usa questa policy predefinita. Le regole arrivano da `governance.rules` di `manent.yml` come dati iniziali, oppure dallo store. Un test di equivalenza (§12) garantisce che `builtin` e `opa` + `manent.lib.rules` producano le stesse decisioni.

## 6. Finestre di freeze come dati

Le finestre restano dati e non diventano regole Rego, per tre motivi:

1. `Freezes` (`GET /api/v1/freezes`, `manent freezes`, UI) deve **elencare** finestre attive e future con la prossima occorrenza. Da una regola Rego arbitraria non si può ricavare.
2. Le espressioni cron e i fusi orari sono già gestiti bene da `robfig/cron`.
3. Un freeze d'emergenza deve essere un'operazione da un comando, senza scrivere né revisionare codice.

Ci sono quindi due origini, unite in `governance.Windows`:

- **statiche**: `governance.freezes` di `manent.yml`, come oggi;
- **dinamiche**: tabella `manent.freeze_windows` (`manent_freeze_windows` su SQLite) gestita da API/CLI/UI, con gli stessi campi più `hard`, `created_by`, `created_at`, `deleted_at`. Con la sorgente `bundle`, `file` o `consul` arrivano anche da `data.manent.freezes` della revisione.

Manent calcola le finestre attive per l'istante e la region dell'input (`FrozenIn`) e le passa in `input.freezes.active`. La policy può solo esentarle (§5.2).

Creare un freeze richiede il permesso nuovo `freeze.write`, verificato su ogni coppia application/environment delle sue `applications`/`environments`: un pattern `*` richiede il permesso su `*`. Eliminarne uno richiede lo stesso permesso. Entrambe le operazioni sono registrate nell'audit.

## 7. Punti di valutazione e determinismo

Regola generale: **una decisione si prende una volta, in un punto persistito, e si riusa**. Ogni decisione registra `revision` (hash della revisione) e `input_sha256`.

| Fase | Dove | Effetto | Registrata in |
|---|---|---|---|
| `submit` | `Engine.govern` (Submit/Promote/Rollback, release composte per ogni deploy) | `deny`, freeze, `requires` (verificati come oggi con `findRelease`), obblighi | `Request.Policy` (nuovo) + `Request.Approval`/`Request.Change` (compatibilità) |
| `validate` | `describeGovernance` | spiegazione: regola, obblighi, `deny`, note, revisione | risposta |
| `start` | step DBOS `config` (`resolveConfig`), con il descrittore effettivo | solo `deny` e freeze: un divieto o un freeze arrivati dopo il submit fermano il deploy (errore permanente) | output dello step `config` (`policy_revision`, `policy_denied`) |
| `stage` | nuovo step `policy@<stadio>#<n>` in `waitFreeze` | freeze e `deny` per region: pausa (`paused-freeze`) o errore | output dello step |
| `tick` | nuovo step `policy` nel workflow `manent.scheduled-deploy` | ogni tick usa la policy in vigore e produce il proprio snapshot | `Request.Policy` del deploy figlio |

### 7.1 Obblighi cambiati dopo il submit

Gli obblighi (`requires`, `approvals`, `change`) si fissano al **submit**. Un'approvazione già data non viene invalidata da una policy più severa arrivata dopo. È il comportamento di oggi con lo snapshot `Request.Approval`, e la revisione registrata rende la scelta verificabile nell'audit.

Una policy più severa vale dal submit successivo. Per chi la vuole subito:

- l'opzione `governance.opa.start: strict` confronta nella fase `start` gli obblighi registrati con quelli nuovi e rifiuta il deploy se ne mancano;
- `manent-server admin policy pending --revision <r>` mostra i deploy in attesa che la nuova revisione avrebbe trattato diversamente.

### 7.2 Compatibilità dei workflow in corso

- `Request.Policy` è un campo nuovo e facoltativo. I workflow già accodati non ce l'hanno e continuano con i percorsi attuali: niente step `policy@…` in `waitFreeze`, niente step `policy` nel tick.
- La scelta del ramo dipende **solo** dalla richiesta persistita, mai dalla configurazione del processo. La sequenza degli step resta identica in recovery (stesso schema di `Request.Notify`).
- Il nuovo step dello stadio sostituisce, solo per le richieste con `Request.Policy`, lo step `now` e la valutazione in memoria. Il suo output contiene istante, finestra attiva e revisione. Lo step `now` resta per i workflow vecchi: il suo output (`time.Time`) non cambia tipo.
- Non cambiano i nomi dei workflow, né quelli degli step esistenti (`config`, `now`, `gate@…`).

## 8. Motore

### 8.1 Interfaccia

`engine.Governance` diventa:

```go
// Evaluator decides the governance obligations of a deploy.
type Evaluator interface {
	// Evaluate returns the decision for the input; the error is not a denial (see Decision.Deny).
	Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error)
	// Windows lists active and upcoming freeze windows (static and dynamic).
	Windows(application, environment string, t time.Time) []governance.WindowStatus
	// Revision identifies the policy in force (hash), for logs and metrics.
	Revision() governance.Revision
}
```

- `governance.Builtin` adatta `Policy.Rule`/`FrozenIn` (comportamento invariato, `Revision` = hash della sezione YAML più le finestre dinamiche).
- `governance/opa.Engine` usa `rego.PreparedEvalQuery` per revisione, ricompilata solo quando la revisione cambia.

I chiamanti (`govern`, `describeGovernance`, `resolveConfig`, `waitFreeze`) costruiscono l'input con un helper comune, `engine.policyInput`, e applicano la decisione con `engine.applyDecision`.

### 8.2 OPA incorporato

- **Capabilities ristrette:** `ast.CapabilitiesForThisVersion()` senza i built-in non deterministici o con effetti:
    - `http.send`, `net.*`;
    - `opa.runtime`, `rand.*`;
    - `time.now_ns`, `uuid.rfc4122`;
    - `crypto.x509.*` con rete, `trace`.

  Una revisione che li usa non si compila e viene rifiutata.
- **Rego v1** e `strict` (variabili e import inutilizzati sono errori).
- **Timeout di valutazione** (`eval_timeout`, default 200ms) col context, più un limite alla dimensione della revisione (`max_bundle_size`, default 4 MiB) e dei dati.
- **Valori come oggetti tipizzati:** l'input è costruito con `ast.InterfaceToValue` da una struct Go.
- **Prestazioni:** dalla documentazione di OPA una query preparata su policy di questa dimensione costa decine di microsecondi. Va comunque misurata: un benchmark in `governance/opa` impedirà regressioni. Le valutazioni sono rare (un submit, un avvio, uno per stadio).
- **Dimensione:** OPA aggiunge circa 15–20 MB al binario `manent-server`, e mai a `manent`: `internal/clientcli/deps_test.go` va esteso con `github.com/open-policy-agent/opa`.

## 9. Policy modificabili a runtime

### 9.1 Configurazione

```yaml
governance:
  engine: opa                 # builtin (default) | opa
  rules: []                   # still valid: initial data for manent.lib.rules
  freezes: []                 # static windows, merged with the dynamic ones
  opa:
    query: data.manent.governance.decision
    source: store             # store | file | bundle | consul
    refresh: 10s              # how often a process looks for a new revision
    max_staleness: 10m        # beyond this, real deploys are refused (see 9.3)
    eval_timeout: 200ms
    start: deny               # deny | strict (see 7.1)
    store:
      review: true            # activation by a different identity (four eyes)
      tests: required         # the revision's *_test.rego must exist and pass
    file:
      dir: /etc/manent/policies   # *.rego + data.json/yaml, reloaded on change
    bundle:
      url: https://policies.example.com/manent/bundle.tar.gz   # or oci://...
      ca_file: /etc/manent/ca.pem
      credentials_ref: vault:secret/data/manent/bundle
      signing: {public_key_ref: file:/etc/manent/bundle.pub, scope: manent}
    consul:
      prefix: manent/policies     # same client and token as envconfig
```

### 9.2 Sorgenti

| Sorgente | Per chi | Come si aggiorna | Revisione |
|---|---|---|---|
| `store` | chi vuole gestire tutto da manent (API, CLI, UI) | `manent policy push` → revisione in bozza → test → approvazione → attivazione | numero progressivo + sha256 |
| `file` | sviluppo, installazioni piccole, ConfigMap Kubernetes | si riscrive la directory; controllo di mtime e dimensione ogni `refresh` (come `auth.TokenFile`) | sha256 del contenuto |
| `bundle` | GitOps: la policy vive in un repository, la CI la testa (`opa test`) e pubblica un bundle firmato | polling con ETag (`If-None-Match`), firma verificata con `bundle.VerifyBundleSignature` | `.manifest.revision` + sha256 |
| `consul` | chi usa già Consul per l'envconfig | blocking query sul prefisso, `ModifyIndex` | `ModifyIndex` + sha256 |

In tutti i casi una revisione è un **insieme immutabile** di moduli Rego e dati. Il caricamento segue sempre la stessa sequenza:

1. parsing;
2. compilazione con le capabilities;
3. **test inclusi** (`*_test.rego`, eseguiti con `tester.Runner`);
4. **valutazione di prova** su un insieme di input di esempio inclusi in manent, per verificare che `decision` sia definita e rispetti lo schema;
5. sostituzione atomica (`atomic.Pointer`) della query preparata.

Se un passaggio fallisce, resta la revisione precedente. Il processo registra un warning, la metrica `manent_policy_load_errors_total` e un evento di audit `policy.rejected`.

### 9.3 Disponibilità e fail-closed

| Situazione | Effetto |
|---|---|
| All'avvio nessuna revisione caricabile | `/readyz` 503 con motivo. L'API rifiuta i deploy reali con 503 (`ErrUnavailable`, temporaneo), il worker non preleva dalla coda. Dry-run, letture e cancel funzionano. |
| Sorgente irraggiungibile ma revisione già caricata | si continua con quella. Oltre `max_staleness` si comporta come il caso precedente; metrica `manent_policy_age_seconds` e allarme. |
| Errore di valutazione (runtime error, timeout, schema) | **rifiuto**, mai via libera. Al submit 422 con `policy evaluation failed: …`. Nello step `config` e nello step dello stadio è un errore temporaneo con retry, perché la correzione è una nuova revisione. |
| `decision` indefinita | come un errore di valutazione. |

Il motore `builtin` non ha sorgente: le finestre dinamiche si leggono dal database con la stessa cache e lo stesso `refresh`. Un database irraggiungibile lascia le ultime finestre lette, entro `max_staleness`.

### 9.4 Ciclo di vita nello store

Tabelle sul database di manent, aperte come quelle di `cluster.Registry`:

- `manent.policy_revisions`: `revision` (seriale), `sha256`, `bundle` (tar.gz), `status` (`draft`, `active`, `superseded`, `rejected`), `author`, `comment`, `reviewed_by`, `created_at`, `activated_at`, `test_report`;
- `manent.policy_active`: la revisione attiva, una riga aggiornata in transazione (`UPDATE … WHERE revision = <letta>`, come la sostituzione condizionata di `Register`);
- `manent.freeze_windows` (§6).

```
manent policy push -d policies/ -m "DBA approval for migrations"   # → revisione 13 (draft), test eseguiti dal server
manent policy diff 12 13                                         # differenze tra moduli e dati
manent policy impact 13 --since 30d                              # decisioni ricalcolate sui deploy reali del periodo
manent policy eval 13 -f deploy.yml [-e prod]                    # decisione per un descrittore
manent policy approve 13                                         # attivazione (identità diversa dall'autore se review: true)
manent policy activate 12                                        # ritorno a una revisione precedente (stesso controllo)
manent policy list | show <r> | pull <r> -d out/
manent freeze add --name incident-881 -e prod -a '*' --until 2026-10-18T18:00:00+02:00 --reason "incident 881" [--hard]
manent freeze list | delete <nome>
```

- **`impact`** ricostruisce gli input dei deploy del periodo dalle richieste registrate (`loadRequest`, inventario, `RequestedBy`), con `now` uguale al submit originale. Poi confronta la decisione registrata con quella della nuova revisione: deploy che sarebbero stati rifiutati, approvazioni in più o in meno. Rende visibile, prima di attivarla, una policy che bloccherebbe tutto.
- **Quattro occhi:** con `store.review: true` l'attivazione richiede il permesso `policy.approve` e un'identità diversa dall'autore della revisione (stessa regola di `Guard.Decide`).
- **Distribuzione:** ogni processo legge `policy_active` ogni `refresh` (una query su una riga). Su Postgres si aggiunge `LISTEN manent_policy`, con un `NOTIFY` all'attivazione, per una propagazione in meno di un secondo. Il polling resta il meccanismo di sicurezza. Una revisione si scarica solo se cambia, e resta in cache per sha256.

### 9.5 Consistenza tra processi

Tra l'attivazione e la ricarica su tutti i nodi passano al più `refresh` secondi, oppure meno di un secondo con NOTIFY. In quell'intervallo un submit sull'API e lo step `config` sul worker possono usare revisioni diverse. È accettabile perché:

- ogni decisione registra la propria revisione (audit esatto);
- la fase `start` applica sempre la revisione del worker per divieti e freeze, quindi un freeze d'emergenza raggiunge anche i deploy accodati un attimo prima;
- per i workflow in corso conta solo ciò che è registrato (§7).

## 10. Sicurezza

- **Nuove azioni RBAC**:
    - `policy.read` e `policy.write` (push, diff, impact, eval);
    - `policy.approve` (attivazione e ritorno);
    - `freeze.write`.

  Le azioni `policy.*` sono di piattaforma, come `platform.read` (`AllowedAnywhere`), e nessun ruolo predefinito tranne `admin` le concede. `freeze.write` va nel ruolo nuovo `release-manager` e in `admin`.
- **Separazione dei poteri:** la policy di governance non decide chi può cambiare la policy. L'RBAC resta in `manent.yml`, così una revisione non può concedersi permessi da sola. Se in futuro l'RBAC passasse in Rego (§13), le azioni `policy.*` resterebbero comunque valutate dalle regole statiche.
- **Break-glass:** `manent-server admin policy activate <r>` e `admin freeze delete` lavorano direttamente sul database con l'identità `cli:<utente OS>`, fuori dal Guard ma registrati nell'audit. Servono se una revisione blocca tutto e l'API non è utilizzabile, o se manca un secondo approvatore durante un incidente.
- **Integrità:**
    - nello store ogni revisione è immutabile e identificata da sha256;
    - nella sorgente `bundle` la firma è obbligatoria salvo `signing: {}` esplicito, segnalato da `Config.SecurityWarnings`;
    - nella sorgente `file` i permessi della directory vengono controllati e segnalati come per il `tokens_file`.
- **Input senza segreti:** l'input contiene solo dati già esposti da `Status`/`Inventory` (versioni, etichette, identità, gruppi). I parametri dei plugin non entrano: possono contenere valori sensibili anche se non sono segreti. Se servisse, si potrà aggiungere un elenco esplicito di parametri esposti (`governance.opa.input_params`).
- **Audit:** `policy.push`, `policy.approve`, `policy.activate`, `policy.rejected`, `freeze.create`, `freeze.delete`, più `governance.decision` per ogni decisione al submit e all'avvio (revisione, `input_sha256`, esito, codici `deny`). Il tutto nella catena di hash esistente e verso il SIEM.

## 11. API, CLI, UI, osservabilità

**API** (da aggiungere ad `api/openapi.yaml`):

| Metodo e percorso | Permesso |
|---|---|
| `GET /api/v1/policies`, `GET /api/v1/policies/:revision[/bundle]` | `policy.read` |
| `POST /api/v1/policies` (tar.gz o JSON con moduli e dati) | `policy.write` |
| `POST /api/v1/policies/:revision/evaluate`, `GET /api/v1/policies/:revision/impact?since=` | `policy.write` |
| `POST /api/v1/policies/:revision/approve`, `POST /api/v1/policies/:revision/activate` | `policy.approve` |
| `POST /api/v1/freezes`, `DELETE /api/v1/freezes/:name` (oltre al `GET` esistente) | `freeze.write` |

I metodi vanno in un nuovo `service.PolicyService`, aggiunto in `service`, `engine`, `authz.Guard`, `api`, `apiclient` e nei servizi finti, come richiesto da CLAUDE.md.

**Validazione e stato**:

- `GovernanceValidation` aggiunge `revision`, `deny`, `notes`, `approvers`;
- `Status` aggiunge `policy: {revision, rule}`;
- le richieste di approvazione mostrano chi può approvare.

**UI**:

- pagina "Policy" con revisioni, diff, report dei test, impatto e approvazione;
- pagina "Freeze" con creazione e chiusura anticipata;
- nel dettaglio del deploy, la decisione con le note.

**Metriche**:

- `manent_policy_revision_info{revision,sha256}`;
- `manent_policy_age_seconds`;
- `manent_policy_load_errors_total`;
- `manent_policy_evaluations_total{phase,outcome}`;
- `manent_policy_eval_duration_seconds{phase}`.

**Log**: in italiano, ad esempio `revisione 13 della policy attiva`, `revisione 14 rifiutata: …`.

**Tracce**: una span `governance.evaluate` figlia della richiesta o dello step.

## 12. Test

- **Equivalenza** `builtin` ↔ `opa` + `manent.lib.rules`: tabella generata con le regole e le finestre dei test esistenti (`governance_test.go`, test dell'engine), più combinazioni casuali di application/environment/glob. Stessa decisione attesa.
- **Unit** di `governance/opa`:
    - schema della decisione;
    - capabilities rifiutate (`http.send`, `time.now_ns`);
    - timeout;
    - decisione indefinita;
    - `freeze_exempt` che non può esentare una finestra `hard`.
- **Engine** (DBOS reale su SQLite, `internal/testdb`):
    - una revisione cambiata tra submit e start, con divieto all'avvio e obblighi invariati;
    - crash del worker durante `waitFreeze` con una revisione cambiata nel frattempo: la recovery rilegge l'output dello step e non rivaluta;
    - workflow accodato senza `Request.Policy` che segue il percorso vecchio;
    - tick di una schedule con la policy in vigore.
- **Store:**
    - attivazione condizionata con due attivazioni concorrenti;
    - quattro occhi;
    - NOTIFY e polling;
    - revisione non valida che non sostituisce quella attiva.
- **Caso d'uso UC23**:
    - push di una policy che richiede 2 approvazioni per le migrazioni;
    - `impact`, approvazione da un secondo utente;
    - freeze d'emergenza durante un rollout a ondate: pausa allo stadio successivo, ripresa a `freeze delete`;
    - bundle firmato servito da `fakes`, con un bundle non firmato rifiutato.

## 13. Estensioni successive

- **`descriptor_policy` in Rego** (`data.manent.descriptor.deny`): stessi punti (`Resolver.Resolve`), input = descrittore e configurazione di ambiente prima della fusione.
- **RBAC in Rego** (`data.manent.authz.allow`): da valutare con attenzione per `List`, che chiama `Allowed` su molte righe. Serve la partial evaluation di OPA per ricavare un filtro, oppure una cache per identità e revisione.
- **Tenant** (proposta precedente): `input.tenant` con nome e tag. Nessun cambio al modello della decisione.
- **Dati esterni senza `http.send`:** un "data provider" in manent aggiorna i dati della revisione a intervalli (calendario delle festività, stato degli incidenti dal sistema ITSM). La valutazione resta pura.

## 14. Fasi

1. **Interfaccia e snapshot**, senza cambio di comportamento:
    - `governance.Evaluator` e `Builtin`;
    - `Request.Policy`;
    - step `policy@<stadio>#<n>` e `policy` del tick per le richieste nuove;
    - revisione nei log e nell'audit.
2. **Freeze dinamici**: tabella, API, CLI, UI, `freeze.write`, `hard`. Già utilizzabili con `builtin`.
3. **Motore OPA** con sorgente `file`:
    - libreria `manent.lib` e `manent.lib.rules`;
    - test di equivalenza;
    - `manent policy eval` locale (`manent-server policy eval --offline`);
    - documentazione EN/IT (`guida/governance`, nuova pagina `guida/policy`).
4. **Store e revisioni**: push, test sul server, diff, impact, quattro occhi, NOTIFY, UI, UC23.
5. **Sorgenti `bundle` (con firma) e `consul`.**
6. **Estensioni** (§13), su richiesta.

Le fasi 1 e 2 danno subito la parte di maggior valore operativo (freeze d'emergenza senza riavvio) e preparano il terreno senza introdurre OPA.

## 15. Alternative considerate

| Alternativa | Perché scartata |
|---|---|
| Ricaricare `manent.yml` a caldo (SIGHUP) | Risolve solo il riavvio, non l'espressività. La configurazione resta diversa nodo per nodo finché non viene distribuita, senza revisioni, test né audit delle modifiche. |
| OPA come servizio remoto (sidecar o centrale, `POST /v1/data/...`) | Una dipendenza di rete in più nel percorso di ogni deploy: latenza, disponibilità, un altro punto da proteggere. La revisione usata da una decisione non è verificabile, se non con `decision_id` e decision log esterni. La gestione centrale si ottiene comunque con i bundle (§9.2), che sono il meccanismo di distribuzione nativo di OPA. |
| CEL (come in Kubernetes) | Più semplice e più veloce, ma senza moduli, test, dati, bundle e firme. OPA/Rego è lo standard già adottato per la policy as code, con strumenti (`opa test`, `opa fmt`, Regal) che le organizzazioni usano già. |
| Cedar | Ottimo per l'autorizzazione, meno adatto a produrre obblighi strutturati (approvazioni, change) invece di allow/deny. |
| Valutare la policy a ogni lettura anche nei workflow | Viola il determinismo di DBOS (§3) e rende inspiegabili le decisioni passate. |

## 16. Questioni aperte

1. **Obblighi più severi dopo il submit:** `start: deny` di default (§7.1) va bene, o la maggior parte delle organizzazioni vorrà `strict`?
2. **Approvatori per gruppo** (`approvals.approvers`): si sommano al permesso `deployment.approve` (proposta) o lo sostituiscono?
3. **Regole YAML con il motore `opa`:** restano in `manent.yml` come dati iniziali, oppure con `source: store` diventano sola lettura e si gestiscono solo dallo store?
4. **Sorgente predefinita per `engine: opa`:** `file` (più semplice) o `store` (più completo)?
5. **Conservazione delle revisioni nello store:** tutte (audit) o le ultime N più quelle referenziate da deploy ancora nella retention (`Admin.Prune`)?

## 17. Implementazione

Il design è implementato per intero nelle fasi 1–5 (§14); la fase 6 (estensioni) resta da fare. Per le questioni aperte (§16) sono state scelte le proposte del documento:

- `start: deny` di default;
- approvatori in aggiunta al permesso `deployment.approve`;
- regole YAML come dati iniziali anche con `opa`;
- `store` come sorgente predefinita;
- revisioni conservate tutte.

Differenze rispetto al testo:

| Punto | Proposta | Implementazione |
|---|---|---|
| `refresh`, `max_staleness`, `start` | sotto `governance.opa` | sotto `governance`: valgono anche per i freeze a runtime del motore `builtin`. |
| Pausa per freeze | sleep fino alla fine della finestra | ricontrollo ogni 15s (`freezeRecheck`), perché una finestra creata a runtime si può chiudere in anticipo. |
| Indisponibilità sul worker | il worker non preleva dalla coda | lo step `config` fallisce con errore temporaneo ed è ritentato; `/readyz` risponde 503. |
| `GET /policies/:revision/bundle` | download del tar.gz | i file sono nel JSON della revisione (`GET /policies/:revision`, `manent policy pull`). |
| Bundle | HTTP(S) o `oci://` | solo HTTP(S), con ETag e firma OPA. |
| Decisioni nell'audit (`governance.decision`) | voce di audit per ogni decisione | riga di log (quindi SIEM) con revisione e hash dell'input; nell'audit le operazioni sulle revisioni e sui freeze. |
| Tabella dei freeze | `deleted_at` | eliminazione della riga; la storia è nell'audit (`freeze.create`, `freeze.delete`). |
| Break-glass | `admin policy activate`, `admin freeze delete` | `admin policy-activate`, `admin freeze-delete`, con `--reason` obbligatorio. |
| Stato della revisione `rejected` | revisioni rifiutate registrate | una revisione che non compila o non passa i test viene rifiutata al caricamento e non viene memorizzata. |

Codice:

- `internal/governance/decision.go`: input, decisione, `Decide`;
- `internal/engine/policy.go`: fasi, snapshot, step `policy@…` e `PolicyService`;
- `internal/policy`: compilazione OPA, libreria Rego, runtime, sorgenti, metriche;
- `internal/cluster/policy.go`: store;
- `internal/authz/policy.go`: permessi;
- API (`internal/api/policy.go`), CLI (`manent policy|freeze`, `manent-server policy`, break-glass), pagine Policy e Calendario della web UI;
- caso d'uso UC23;
- documentazione `guida/policy`.

