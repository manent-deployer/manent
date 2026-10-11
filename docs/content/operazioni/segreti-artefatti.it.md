# Segreti e sorgenti degli artefatti

## Provider di segreti

Descrittore, configurazione di ambiente e configurazione di manent contengono solo **riferimenti** ai segreti, nella forma `<schema>:<riferimento>`. Il valore viene letto dal worker solo quando serve e non viene mai salvato.

| Schema | Esempio | Valore |
|---|---|---|
| `env:` | `env:DEPLOY_SSH_KEY` | Variabile d'ambiente del worker. |
| `file:` | `file:/run/secrets/db` | Contenuto di un file sul worker. |
| `vault:` | `vault:secret/data/myapp/prod#ssh_key` | Campo di un segreto di HashiCorp Vault. |

### HashiCorp Vault

```yaml title="manent.yml"
secrets:
  vault:
    enabled: true
    address: https://vault.example.com:8200   # default VAULT_ADDR
    namespace: ""                            # Vault Enterprise / HCP
    auth: approle
    approle:
      role_id: manent
      secret_id_file: /etc/manent/vault-secret-id
    ca_file: /etc/ssl/certs/corp-ca.pem
    timeout: 10s
```

Il riferimento è `vault:<percorso API>#<campo>`:

- **KV v2:** il percorso include `data/`, come nell'API di Vault: `vault:secret/data/myapp/prod#ssh_key`;
- **KV v1:** `vault:kv/myapp/prod#password`.

manent riconosce la risposta di KV v2 e legge il campo da `data.data`. Il campo deve essere una stringa.

| `auth` | Credenziali |
|---|---|
| `token` (default) | `token_file`, riletto a ogni uso (ad esempio aggiornato da Vault Agent), oppure la variabile `VAULT_TOKEN`. |
| `approle` | `approle.role_id` e `approle.secret_id_file`. `approle.mount` default `approle`. |
| `kubernetes` | `kubernetes.role` e il token del service account (`kubernetes.jwt_file`, default `/var/run/secrets/kubernetes.io/serviceaccount/token`). `kubernetes.mount` default `kubernetes`. |

Con `approle` e `kubernetes` manent fa il login, tiene il token in memoria e lo sostituisce all'80% della sua durata. Se Vault rifiuta il token (revoca), rifà il login una volta. Gli errori riportano lo status e i messaggi di Vault, mai il valore dei segreti.

La policy Vault del ruolo di manent deve concedere `read` solo sui percorsi necessari. Combinata con `descriptor_policy.secret_refs` limita ciò che ogni applicazione può leggere:

```yaml
descriptor_policy:
  secret_refs: ["vault:secret/data/{application}/*"]
```

## Sorgenti degli artefatti

Il worker scarica l'artefatto da `artifact.source`: un percorso locale, `file://` o `http(s)://`. La sezione `artifacts` stabilisce quali sorgenti sono ammesse e con quali credenziali.

```yaml title="manent.yml"
artifacts:
  timeout: 30m
  allow_unlisted: false
  sources:
    - prefix: https://nexus.example.com/repository/releases/
      auth:
        type: basic
        username: manent
        secret_ref: vault:secret/data/manent/nexus#password
    - prefix: https://artifactory.example.com/artifactory/
      auth:
        type: header
        header: X-JFrog-Art-Api
        secret_ref: vault:secret/data/manent/artifactory#api_key
      ca_file: /etc/ssl/certs/corp-ca.pem
    - prefix: file:///srv/artifacts/
```

| Chiave | Descrizione |
|---|---|
| `timeout` | Durata massima di un download (default `30m`). |
| `allow_unlisted` | `true` (default): sono ammesse anche le sorgenti non elencate, senza credenziali. `false`: solo quelle che corrispondono a un `prefix`. |
| `sources[].prefix` | Prefisso dell'URL: `https://`, `http://` o `file:///`. Se più prefissi corrispondono vince il più lungo. |
| `sources[].auth.type` | `basic` (con `username`), `bearer`, `header` (con `header`). |
| `sources[].auth.secret_ref` | Riferimento del segreto (password, token o API key), letto a ogni download. |
| `sources[].ca_file` | CA aggiuntive per la sorgente. |

Le credenziali stanno nella configurazione di manent, non nel descrittore: chi richiede un deploy indica solo l'URL dell'artefatto.

- **Percorsi locali.** Un percorso viene confrontato come `file://` più il percorso assoluto ripulito, quindi `../` non permette di uscire da un prefisso ammesso.
- **Quando viene verificata.** La sorgente è controllata all'accodamento (`422` se non ammessa) e di nuovo dal worker al download.
- **Redirect.** Se la sorgente reindirizza verso un altro host, le credenziali non vengono inoltrate.

!!! warning "Perché limitare le sorgenti"
    Con `allow_unlisted: true` un descrittore può far scaricare al worker qualsiasi URL raggiungibile dalla sua rete, o copiare sui target un file locale del worker, ad esempio `/etc/passwd`. In produzione impostare `allow_unlisted: false`.
