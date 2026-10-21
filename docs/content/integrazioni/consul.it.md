# HashiCorp Consul

La [configurazione di ambiente](../guida/configurazione-ambiente.md) di ogni applicazione e ambiente (plugin, target, parametri, riferimenti ai segreti, strategia di rilascio) può stare nel KV di Consul. manent la legge all'avvio di ogni deploy e registra quale revisione ha usato, così ogni deploy si può ricondurre alla configurazione esatta che l'ha prodotto.

!!! abstract "Cosa serve"
    Un cluster Consul con le ACL attive · i permessi per creare policy e token · la CLI `consul`

## 1. Struttura delle chiavi

| Chiave | Contenuto |
|---|---|
| `manent/<applicazione>/<ambiente>` | Configurazione di ambiente, YAML o JSON. |

Il modello si può cambiare (`envconfig.consul.key`).

!!! danger "Riferimenti, mai valori"
    La configurazione che manent legge viene salvata nello storico dei deploy. Deve contenere solo riferimenti ai segreti (`vault:…`, `file:…`), mai password o chiavi.

## 2. ACL: manent legge, i responsabili scrivono

```hcl title="manent-read.hcl"
key_prefix "manent/" {
  policy = "read"
}
```

```sh
consul acl policy create -name manent-read -rules @manent-read.hcl
consul acl token create -description "manent workers and API" -policy-name manent-read
```

Chi scrive riceve policy proprie e più ristrette, per esempio una per responsabile di applicazione:

```hcl title="shop-owners.hcl"
key_prefix "manent/shop/" {
  policy = "write"
}
```

## 3. Configura manent

```yaml title="manent.yml"
envconfig:
  provider: consul
  required: true               # un ambiente senza configurazione fallisce invece di usare solo il descrittore
  max_retries: 3               # tentativi aggiuntivi se Consul non risponde
  consul:
    address: https://consul.example.com:8501    # default CONSUL_HTTP_ADDR o http://127.0.0.1:8500
    datacenter: ""
    key: "manent/{application}/{artifact}/{environment}"
    token_file: /run/secrets/consul-token        # oppure CONSUL_HTTP_TOKEN
```

Il token ACL non si scrive mai in `manent.yml`: arriva da `CONSUL_HTTP_TOKEN` o da `token_file`. Serve sia all'API (per verificare gli invii) sia ai worker (per eseguire i deploy).

## 4. Scrivi una configurazione

```sh
consul kv put manent/shop/shop/prod @envconfig/shop/shop/prod.yml
consul kv get -detailed manent/shop/shop/prod | grep ModifyIndex
```

```sh
manent validate -f deploy/prod.yml | jq .config
```

```json
{"source": "consul:manent/shop/shop/prod", "revision": "4127", "targets": "6"}
```

`revision` è il `ModifyIndex` della chiave. L'output dello step `config` di ogni deploy riporta la stessa coppia, quindi alla domanda "quale configurazione ha usato il deploy X?" c'è una risposta precisa.

## 5. Configurazione come codice

Tieni la configurazione in un repository e lascia che sia una pipeline a scriverla, dopo la verifica di manent:

1. **Merge request:** `manent env validate -a shop -e prod -f envconfig/shop/shop/prod.yml` esegue tutti i controlli dell'invio (fusione con il deploy in esercizio, policy sui descrittori, piano del rollout, plugin) e non scrive nulla. Vedi [GitHub Actions](github-actions.md) o [GitLab CI](gitlab-ci.md).
2. **Merge:** la pipeline esegue `consul kv put`, oppure Terraform con `consul_keys` condizionato al data source [`manent_environment_check`](terraform.md).

## Quando viene letta

- **All'invio**, per rifiutare subito un conflitto, un parametro non ammesso o un ambiente senza target (`422`). Se Consul non risponde, la verifica è lasciata al worker.
- **All'avvio del deploy**, nello step `config`, con `max_retries`. Un deploy pianificato usa quindi la configurazione valida quando gira. Una volta letta, la configurazione viene salvata con il deploy: un deploy ripreso dopo un crash la riusa anche se nel frattempo la chiave è cambiata.

Riferimento: [Configurazione di ambiente](../guida/configurazione-ambiente.md).
