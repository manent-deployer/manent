# Configurazione di ambiente

Plugin, target, parametri e riferimenti ai segreti di ogni applicazione in ogni ambiente possono essere gestiti in un sistema di configurazione invece che nel descrittore. Chi richiede il deploy indica solo applicazione, ambiente e artefatto:

```yaml title="Descrittore minimale"
apiVersion: manent/v1
application: myapp
environment: test
artifact:
  type: application
  name: myapp
  version: 1.2.3
  source: https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz
```

## Formato

La configurazione di un ambiente è un documento YAML o JSON:

```yaml title="manent/untenanted/myapp/myapp/test"
plugin: remote-copy
targets:
  - host: 192.168.1.10
    port: 22
    user: deploy
    labels: {az: a}
  - host: 192.168.1.11
    user: deploy
    labels: {az: b}
params:
  dest_dir: /opt/myapp/releases
  ssh_key_secret: deploy-key
  post_command: "systemctl restart myapp"
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY
rollback:                 # opzionale: vedi Rollback
  enabled: true
rollout:                  # opzionale: vedi Rilasci a ondate
  waves:
    - {name: first, select: "az=a", bake: 5m}
    - {name: rest, select: "az=b"}
```

!!! danger "Solo riferimenti ai segreti"
    La configurazione letta viene salvata nello storico del deploy. Deve contenere solo **riferimenti** ai segreti (`env:…`, `file:…`), mai i valori.

## Proprietà { #proprieta }

Un documento accetta solo queste chiavi: una chiave sconosciuta (un errore di battitura, o una chiave del descrittore come `deploy` o `artifact`) rende il documento non valido, e il deploy fallisce nello step `config` (con Consul e file dopo i tentativi di `max_retries`). Tutte le chiavi sono facoltative; come ciascuna si combina con il descrittore è in [Regole di fusione](#regole-di-fusione).

| Chiave | Tipo | Descrizione |
|---|---|---|
| `plugin` | stringa | Plugin di deploy (`deploy.plugin` del descrittore). Assente: il primo plugin attivo che supporta `artifact.type`, in ordine alfabetico. |
| `targets` | lista | Destinazioni del deploy: `host` (obbligatorio), `port`, `user`, `labels` (mappa di stringhe; `region` e `az` sono usate dai [rilasci a ondate](ondate.md), `slot` dal router dei [rilasci per percentuale](traffico.md)). |
| `params` | mappa di stringhe | Parametri del plugin; vedi la pagina di ciascun [plugin](../plugins/remote-copy.md). I nomi dei segreti (`*_secret`) si riferiscono a `secrets`. |
| `secrets` | lista | Segreti usabili dal plugin: `name` (usato in `params`) e `ref`, un riferimento come `env:…`, `file:…` o `vault:…`, mai un valore. |
| `parallel` | stringa | Target di un lotto che il plugin aggiorna nello stesso momento: un numero (`"4"`) o una percentuale del lotto (`"25%"`). Vedi [target in parallelo](ondate.md#target-in-parallelo). |
| `rollback` | oggetto | [Rollback](rollback.md) automatico: `enabled`, `on` (step il cui fallimento lo avvia), `locator`; entro quanto ammette il tipo di artefatto. |
| `rollout` | oggetto | Strategia dei [rilasci a ondate](ondate.md): `waves`, `maxUnavailable`, `bake`, `health`, `onFailure`, `strategy` e gli altri campi del `rollout` del descrittore, con gli stessi nomi. |
| `step_params` | mappa di mappe di stringhe | Parametri degli [step personalizzati](step-personalizzati.md), per nome dello step: `{migrate: {schema: shop}}`. |
| `traffic` | oggetto | Load balancer e router dell'ambiente: `pool` per lo [svuotamento](svuotamento.md), `router` per i [rilasci per percentuale](traffico.md). Solo qui: un descrittore non può impostarlo. |
| `imports` | mappa | Release in esercizio di cui l'ambiente usa gli export: `{infra: {current: {application: infra}}}` (con `tenant`, `artifact`, `environment` facoltativi). Solo `current`: un import `deployment` appartiene al descrittore. Vedi [Export e import](catene.md#export-import). |
| `targets_from` | lista | Target dagli export `targets` degli import: `[{import: infra, export: vms}]`. |
| `params_from` | mappa | Parametri del plugin dagli export stringa degli import: `{db_host: {import: infra, export: db_host}}`. |
| `exports` | mappa | Output dei deploy che altri deploy possono importare: `from` (`<step>.<chiave>`), `format` (`string` o `targets`), `pattern` (obbligatorio per `string`), `public` (leggibile da altri tenant). |

Le chiavi della configurazione di ambiente sono in `snake_case` (`step_params`, `targets_from`, `params_from`), quelle del descrittore in `camelCase` (`stepParams`, `deploy.targetsFrom`): i valori hanno la stessa struttura. La coda su cui gira un deploy non è tra le proprietà (vedi la fine delle [Regole di fusione](#regole-di-fusione)).

## Provider

Si configura in `manent.yml`, sezione `envconfig`: ogni voce di `providers` ha un `name`, un `type` e la sezione del tipo. Con un solo provider il nome è libero (`default` negli esempi); per più provider vedi [Più provider](#piu-provider).

=== "Consul"

    ```yaml
    envconfig:
      required: true
      max_retries: 3
      providers:
        - name: default
          type: consul
          consul:
            address: http://consul:8500            # default: CONSUL_HTTP_ADDR o http://127.0.0.1:8500
            datacenter: ""
            key: "manent/{tenant}/{application}/{artifact}/{environment}"
            # token_file: /run/secrets/consul-token
    ```

    La configurazione è il valore della chiave KV. Il token ACL si legge da `CONSUL_HTTP_TOKEN` oppure da `token_file`, mai dal file di configurazione.

    ```sh
    consul kv put manent/untenanted/myapp/myapp/test @examples/envconfig/untenanted/myapp/myapp/test.yml
    ```

=== "Git"

    ```yaml
    envconfig:
      providers:
        - name: default
          type: git
          git:
            repository: https://git.example.com/platform/envconfig.git
            ref: refs/heads/main
            path: "{tenant}/{application}/{artifact}/{environment}.yml"
            password_ref: vault:secret/manent/git#token
    ```

    Il file viene letto a un commit: il suo SHA viene registrato sul deploy, e un descrittore può scegliere una revisione entro `descriptor_policy.config_ref`. Vedi [Repository Git](../integrazioni/git.md).

=== "File"

    ```yaml
    envconfig:
      providers:
        - name: default
          type: file
          file:
            path: "./envconfig/{tenant}/{application}/{artifact}/{environment}.yml"
    ```

    Utile per sviluppo e test.

=== "Nessuno"

    ```yaml
    envconfig:
      providers: []
    ```

    Tutto arriva dal descrittore.

| Opzione | Descrizione |
|---|---|
| `required` | `true`: il deploy fallisce se per l'ambiente non esiste configurazione. `false`: si usa solo il descrittore. |
| `max_retries` | Tentativi aggiuntivi se il provider non risponde (es. Consul non raggiungibile). |

I template (`consul.key`, `file.path`, `git.path` di ogni provider) contengono `{tenant}` (il [tenant](tenant.md) della release, `untenanted` per le applicazioni che nessun tenant elenca) e `{artifact}`, l'`artifact.name` del descrittore: un documento per artefatto di un'applicazione di un tenant. Vedi [Applicazioni con più artefatti](artefatti.md).

## Più provider { #piu-provider }

Un'installazione può leggere da più sistemi insieme: per esempio il team dei pagamenti dal proprio repository Git, le applicazioni non ancora migrate dal vecchio Consul, tutte le altre dal Consul di default. Ogni sistema è una voce di `envconfig.providers`:

```yaml title="manent.yml"
envconfig:
  required: true
  providers:
    - name: payments-git
      type: git
      git:
        repository: https://git.example.com/payments/envconfig.git
        password_ref: vault:secret/manent/payments-git#token
      match: {applications: ["payments/*"]}
    - name: legacy-consul
      type: consul
      consul:
        address: https://consul-old.example.com
        key: "manent/{application}/{environment}"
        token_file: /etc/manent/consul-old.token
      match: {environments: [dev, test]}
      on_missing: next                     # non ancora migrata? leggi il successivo
    - name: retail-git
      type: git
      git: {repository: https://git.example.com/retail/envconfig.git}
      tenant_only: true
    - name: default
      type: consul
```

```yaml title="tenants.yml"
tenants:
  retail:
    applications: {shop: {}}
    envconfig: retail-git                  # letto per primo, qualunque cosa dicano i match
```

- **Quale provider.** Un deploy legge dal provider che il suo tenant indica nel registro (`envconfig`), poi dai provider il cui `match` corrisponde, in ordine. `match` accetta pattern su `applications` (qualificati con il tenant, come nei permessi e nelle regole: `payments/*`), `artifacts` ed `environments`; un provider senza `match` serve tutto, uno con `tenant_only: true` solo i tenant che lo indicano. Vince il primo che ha il documento; senza candidati il deploy non ha configurazione di ambiente (un errore con `required: true`).
- **Documento assente.** Con `on_missing: fail` (default) il deploy si ferma a quel provider, come con uno solo. Con `on_missing: next` passa al candidato successivo: utile per migrare un'applicazione alla volta. Ogni altro errore, come un sistema irraggiungibile o un documento non valido, non passa mai al successivo: il deploy ritenta, e non legge mai in silenzio un'altra configurazione.
- **Impostazioni proprie.** Ogni provider ha modello, indirizzo e credenziali. `CONSUL_HTTP_TOKEN` e `CONSUL_HTTP_ADDR` sono solo i default dei provider Consul senza `token_file` e `address`.
- **Tracciabilità.** Lo step `config` registra `provider` (e `skipped`, i provider saltati con `on_missing: next`) accanto a `source` e `revision`; con Git il deploy registra anche `config_provider`, mostrato nello stato accanto a `config_revision`.
- **Revisioni.** `config.ref` richiede che il provider da cui il deploy legge sia Git. Un rollback manuale e la riconciliazione rileggono la revisione registrata dal provider che l'ha registrata, anche se le regole ora ne sceglierebbero un altro: uno SHA appartiene a un solo repository. Se quel provider non è più configurato, il deploy fallisce con un errore permanente.
- **Nomi sconosciuti.** Un tenant che indica un provider inesistente viene segnalato nel log all'avvio, e i suoi deploy falliscono.
- **Versioni precedenti.** `envconfig.provider` con accanto la sezione `consul`, `git` o `file` non è più accettato: manent non parte e l'errore mostra la forma con `providers`. Sposta la sezione in una voce, per esempio `providers: [{name: default, type: consul, consul: {...}}]`.

## Quando viene letta

La configurazione viene letta all'inizio di ogni deploy, nello step `config`. Ne seguono due conseguenze:

- un deploy pianificato usa la configurazione valida al momento dell'esecuzione, non quella del momento in cui è stato pianificato;
- una volta letta, viene salvata da DBOS. Se il deploy viene ripreso dopo un crash, riusa la stessa configurazione anche se nel frattempo è cambiata.

L'output dello step `config` riporta la fonte (`source`, es. `consul:manent/untenanted/myapp/myapp/test`) e la revisione (`revision`: il `ModifyIndex` di Consul, o lo SHA del commit con Git), così ogni deploy è riconducibile alla configurazione esatta che ha usato.

## Regole di fusione { #regole-di-fusione }

La configurazione di ambiente è **autorevole**: il descrittore può solo aggiungere ciò che non vi è definito.

| Campo | Regola |
|---|---|
| `plugin`, `parallel` | Se definiti in entrambi devono coincidere. `parallel` è `deploy.parallel`: i target di un lotto aggiornati nello stesso momento ([target in parallelo](ondate.md#target-in-parallelo)); un valore definito qui non è soggetto a `descriptor_policy.max_parallel`. |
| `targets` | Se la configurazione li definisce, il descrittore può ometterli o ripeterli identici, etichette comprese. |
| `params` | Il descrittore può aggiungere chiavi; una chiave presente in entrambi deve avere lo stesso valore. |
| `secrets` | Il descrittore può aggiungere segreti; lo stesso nome deve avere lo stesso riferimento. |
| `step_params` | Parametri degli [step personalizzati](step-personalizzati.md), per step: il descrittore (`stepParams`) può aggiungere le chiavi ammesse da `descriptor_policy.step_params`; una chiave presente in entrambi deve avere lo stesso valore. |
| `rollout` | La strategia di [rilascio a ondate](ondate.md) dell'ambiente è autorevole: il descrittore può ometterla o ripeterla identica. |
| `traffic` | Load balancer e router dell'ambiente ([svuotamento](svuotamento.md), [rilasci per percentuale](traffico.md)): solo nella configurazione di ambiente, un descrittore che la definisce è rifiutato. |
| `imports`, `targets_from`, `params_from` | Target e parametri del plugin presi dagli export della release in esercizio di un'altra applicazione (solo import `current`), risolti all'avvio di ogni deploy: vedi [Export e import](catene.md#export-import). Con `targets_from` il descrittore non può aggiungere target propri; un parametro in `params_from` non può essere impostato anche dal descrittore. |
| `exports` | Output dei deploy dell'ambiente che altri deploy possono importare; il descrittore può aggiungerne altri, lo stesso nome deve avere la stessa definizione. |
| `rollback` | `enabled`, `on` e `locator` seguono la stessa regola: se definiti in entrambi devono coincidere. Così un ambiente può imporre il [rollback](rollback.md) senza che il descrittore possa disattivarlo. Resta valido il limite `allowed` del tipo di artefatto. |

Un valore in conflitto fa fallire il deploy: non viene mai sovrascritto in silenzio. Dopo la fusione (e gli import) deve esserci almeno un target.

Oltre a queste regole, `descriptor_policy` in `manent.yml` limita ciò che il descrittore può aggiungere: ad esempio può vietare comandi (`post_command`), riferimenti a segreti fuori dal perimetro dell'applicazione e target non definiti dall'ambiente. Vedi [Sicurezza](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

La coda su cui gira un deploy non fa parte della configurazione di ambiente: si decide all'accodamento, prima che questa configurazione venga letta, con la sezione [`queues`](../operazioni/configurazione.md#queues) di `manent.yml` (per tenant/applicazione, artefatto e ambiente), con il [tenant](tenant.md) o con il plugin. Vedi [code instradate](../operazioni/scalabilita.md#routed-queues).

## Verificare il deploy effettivo

```sh
manent validate -f examples/descriptor-envconfig.yml
```

Il server mostra il piano, la fonte e la revisione della configurazione (`config`), la sezione `deploy` effettiva e i nomi dei segreti. Serve il permesso `deployment.read` sull'applicazione e sull'ambiente.

Per provare una configurazione di manent prima di installarla, senza un server in esecuzione, c'è `manent-server -c manent.yml validate -f <descrittore>`, che legge la configurazione di ambiente in locale (`--offline` per non leggerla). Vedi [manent-server](../operazioni/server.md#validate).
