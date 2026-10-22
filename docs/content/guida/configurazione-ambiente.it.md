# Configurazione di ambiente

Plugin, policy, target, parametri e riferimenti ai segreti di ogni applicazione in ogni ambiente possono essere gestiti in un sistema di configurazione invece che nel descrittore. Chi richiede il deploy indica solo applicazione, ambiente e artefatto:

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

```yaml title="manent/myapp/myapp/test"
plugin: remote-copy
policy: default
targets:
  - host: 192.168.1.10
    port: 22
    user: deploy
params:
  dest_dir: /opt/myapp/releases
  ssh_key_secret: deploy-key
  post_command: "systemctl restart myapp"
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY
rollback:                 # opzionale: vedi Rollback
  enabled: true
```

!!! danger "Solo riferimenti ai segreti"
    La configurazione letta viene salvata nello storico del deploy. Deve contenere solo **riferimenti** ai segreti (`env:…`, `file:…`), mai i valori.

## Provider

Si configura in `manent.yml`, sezione `envconfig`:

=== "Consul"

    ```yaml
    envconfig:
      provider: consul
      required: true
      max_retries: 3
      consul:
        address: http://consul:8500            # default: CONSUL_HTTP_ADDR o http://127.0.0.1:8500
        datacenter: ""
        key: "manent/{tenant}/{application}/{artifact}/{environment}"
        # token_file: /run/secrets/consul-token
    ```

    La configurazione è il valore della chiave KV. Il token ACL si legge da `CONSUL_HTTP_TOKEN` oppure da `token_file`, mai dal file di configurazione.

    ```sh
    consul kv put manent/untenanted/myapp/myapp/test @examples/envconfig/myapp/myapp/test.yml
    ```

=== "Git"

    ```yaml
    envconfig:
      provider: git
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
      provider: file
      file:
        path: "./envconfig/{tenant}/{application}/{artifact}/{environment}.yml"
    ```

    Utile per sviluppo e test.

=== "Nessuno"

    ```yaml
    envconfig:
      provider: none
    ```

    Tutto arriva dal descrittore.

| Opzione | Descrizione |
|---|---|
| `required` | `true`: il deploy fallisce se per l'ambiente non esiste configurazione. `false`: si usa solo il descrittore. |
| `max_retries` | Tentativi aggiuntivi se il provider non risponde (es. Consul non raggiungibile). |

I template (`consul.key`, `file.path`, `git.path`) contengono `{tenant}` (il [tenant](tenant.md) della release, `untenanted` per le applicazioni che nessun tenant elenca) e `{artifact}`, l'`artifact.name` del descrittore: un documento per artefatto di un'applicazione di un tenant. Vedi [Applicazioni con più artefatti](artefatti.md).

## Quando viene letta

La configurazione viene letta all'inizio di ogni deploy, nello step `config`. Ne seguono due conseguenze:

- un deploy pianificato usa la configurazione valida al momento dell'esecuzione, non quella del momento in cui è stato pianificato;
- una volta letta, viene salvata da DBOS. Se il deploy viene ripreso dopo un crash, riusa la stessa configurazione anche se nel frattempo è cambiata.

L'output dello step `config` riporta la fonte (`source`, es. `consul:manent/untenanted/myapp/myapp/test`) e la revisione (`revision`: il `ModifyIndex` di Consul, o lo SHA del commit con Git), così ogni deploy è riconducibile alla configurazione esatta che ha usato.

## Regole di fusione

La configurazione di ambiente è **autorevole**: il descrittore può solo aggiungere ciò che non vi è definito.

| Campo | Regola |
|---|---|
| `policy`, `plugin` | Se definiti in entrambi devono coincidere. |
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

## Verificare il deploy effettivo

```sh
manent validate -f examples/descriptor-envconfig.yml
```

Il server mostra il piano, la fonte e la revisione della configurazione (`config`), la sezione `deploy` effettiva e i nomi dei segreti. Serve il permesso `deployment.read` sull'applicazione e sull'ambiente.

Per provare una configurazione di manent prima di installarla, senza un server in esecuzione, c'è `manent-server -c manent.yml validate -f <descrittore>`, che legge la configurazione di ambiente in locale (`--offline` per non leggerla). Vedi [manent-server](../operazioni/server.md#validate).
