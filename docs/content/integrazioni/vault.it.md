# HashiCorp Vault

manent legge i segreti dal motore KV di Vault, versione 1 o 2, tramite l'API HTTP: chiavi SSH, credenziali cloud, token delle chat, password degli ITSM, la chiave HMAC dell'audit. Descrittori, configurazioni di ambiente e `manent.yml` contengono solo riferimenti come `vault:secret/data/shop/prod#ssh_key`; il worker li risolve al momento dell'uso e non salva mai il valore.

!!! abstract "Cosa serve"
    Un server Vault (Community, Enterprise o HCP) · i permessi per creare una policy e un ruolo di autenticazione · il motore KV montato, `secret/` (v2) negli esempi

## 1. Organizza i percorsi

Una struttura per applicazione e ambiente fa lavorare insieme la policy di Vault e la `descriptor_policy` di manent:

```text
secret/manent/…                    # credenziali di manent stesso: slack, servicenow, nexus, audit
secret/<applicazione>/<ambiente>   # un segreto per ambiente, un campo per credenziale
```

```sh
vault kv put secret/shop/prod ssh_key=@deploy_key db_password='…'
vault kv put secret/manent/slack bot_token='xoxb-…' signing_secret='…'
```

## 2. Una policy in sola lettura per manent

```hcl title="manent.hcl"
# KV v2: il percorso dell'API comprende data/
path "secret/data/shop/*"   { capabilities = ["read"] }
path "secret/data/manent/*" { capabilities = ["read"] }
```

```sh
vault policy write manent manent.hcl
```

Concedi `read` solo su ciò che manent deve leggere. Con molte applicazioni usa un glob come `secret/data/+/prod` o una policy per team; a manent non servono mai `list`, `create` o `update`.

## 3. Scegli come si autentica manent

=== "Kubernetes (manent in un cluster)"

    ```sh
    vault auth enable kubernetes
    vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc
    vault write auth/kubernetes/role/manent \
      bound_service_account_names=manent bound_service_account_namespaces=manent \
      token_policies=manent token_ttl=1h
    ```

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: kubernetes
        kubernetes:
          role: manent
          # jwt_file: /var/run/secrets/kubernetes.io/serviceaccount/token   # default
          # mount: kubernetes                                                # default
    ```

    Il chart Helm crea la service account `manent` a cui è legato il ruolo.

=== "AppRole (VM)"

    ```sh
    vault auth enable approle
    vault write auth/approle/role/manent token_policies=manent token_ttl=1h secret_id_ttl=720h
    vault read -field=role_id auth/approle/role/manent/role-id
    vault write -f -field=secret_id auth/approle/role/manent/secret-id > /etc/manent/vault-secret-id
    chmod 600 /etc/manent/vault-secret-id; chown manent /etc/manent/vault-secret-id
    ```

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: approle
        approle:
          role_id: <role_id>
          secret_id_file: /etc/manent/vault-secret-id
          # mount: approle      # default
    ```

=== "Token (Vault Agent)"

    ```yaml title="manent.yml"
    secrets:
      vault:
        enabled: true
        address: https://vault.example.com:8200
        auth: token
        token_file: /run/vault/token   # riletto a ogni uso; oppure la variabile VAULT_TOKEN
    ```

    Lascia a Vault Agent (o a un altro sidecar) il login e l'aggiornamento del file del token: manent lo rilegge ogni volta.

Con `approle` e `kubernetes` manent esegue il login, tiene il token in memoria e lo rinnova all'80% del lease; se Vault lo rifiuta (revoca), rifà il login una volta. Altre opzioni: `namespace` (Vault Enterprise e HCP), `ca_file` per una CA privata, `timeout` (default 10s).

## 4. Usa i riferimenti

```yaml title="envconfig/shop/shop/prod.yml"
secrets:
  - name: deploy-key
    ref: "vault:secret/data/shop/prod#ssh_key"     # KV v2: data/ nel percorso
  # - name: legacy
  #   ref: "vault:kv/shop/prod#password"            # KV v1: senza data/
params:
  ssh_key_secret: deploy-key
```

La stessa sintassi vale in `manent.yml` (`bot_token_ref`, `password_ref`, `secret_ref`, `hmac_key_ref`…). manent riconosce una risposta KV v2 e legge il campo da `data.data`; il campo deve essere una stringa (una chiave SSH o un certificato vanno bene come stringa su più righe).

Limita ciò che i descrittori possono riferire, così un'applicazione non può chiedere i segreti di un'altra:

```yaml title="manent.yml"
descriptor_policy:
  secret_refs: ["vault:secret/data/{application}/*"]
```

## 5. Verifica

```sh
manent deploy -f deploy/prod.yml --dry-run
```

Lo step `secrets` risolve ogni segreto dichiarato e ne riporta il numero (`count`), mai i valori. Un campo mancante, un percorso negato o un login fallito fanno fallire lo step con lo stato e il messaggio di Vault, per esempio un `403` con `permission denied`.

## Rotazione

I valori vengono letti a ogni uso, quindi ruotare un segreto in Vault ha effetto dalla lettura successiva, anche dentro un deploy in corso, senza riavvii né modifiche ai riferimenti. Per ruotare il `secret_id` di AppRole scrivine uno nuovo in `secret_id_file`: viene letto al login successivo.

Riferimento: [Segreti e sorgenti degli artefatti](../operazioni/segreti-artefatti.md#hashicorp-vault).
