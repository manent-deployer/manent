# Svuotamento dal load balancer

Un deploy in place su server o VM dietro un load balancer interrompe le richieste in corso e manda traffico a un nodo che sta ripartendo. Con un **pool** nella configurazione di ambiente manent toglie ogni lotto di un [rilascio a ondate](ondate.md) dal load balancer prima di aggiornarlo e lo rimette in servizio solo quando il load balancer lo vede sano.

```yaml title="envconfig/shop/prod.yml"
rollout:
  waves:
    - {name: canary, select: "canary=true"}
    - {name: resto, select: "canary!=true"}
  maxUnavailable: "1"
traffic:
  pool:
    provider: haproxy
    member: "{host}"            # nome del server nel backend
    drain_timeout: 5m           # attesa massima delle connessioni residue
    min_healthy: "66%"          # membri sani da tenere sempre in servizio
    on_timeout: proceed         # proceed | fail
    settings:
      dataplane_urls: "https://lb1.internal:5555,https://lb2.internal:5555"
      backend: shop
      credentials_secret: haproxy-dataplane
secrets:
  - {name: haproxy-dataplane, ref: "vault:secret/data/lb#dataplane"}
```

Il pool è infrastruttura: sta **solo** nella configurazione di ambiente. Un descrittore con la sezione `traffic` viene rifiutato. Il pool richiede un rilascio a ondate: senza lotti tutti i target verrebbero svuotati insieme, e il deploy fallisce nello step `config`.

## Sequenza di un lotto

Per ogni lotto, intorno agli step da `deploy` ad `activation`:

1. **`drain@<passo>#<lotto>`** legge lo stato del pool e verifica la capacità: se togliendo il lotto i membri sani in servizio scenderebbero sotto `min_healthy` (per esempio perché un altro nodo è già fuori per un guasto), lo svuotamento viene rimandato. manent attende `poll_interval` e riprova (`drain@…#0.1`, `#0.2`, …) fino a `drain_timeout`, poi il passo fallisce. È il controllo che impedisce a un deploy di finire di spegnere un servizio già degradato.
2. **`drain-wait@…`** attende la fine delle connessioni residue, con letture e pause durevoli: un'attesa di cinque minuti sopravvive a un crash del worker. Allo scadere di `drain_timeout`, con `on_timeout: proceed` il deploy prosegue (le connessioni residue vengono chiuse dal riavvio); con `fail` il lotto torna in servizio senza essere toccato e il passo fallisce.
3. Gli step del lotto: `deploy`, `post-test` e gli step personalizzati, con il nodo fuori dal traffico.
4. **`restore@…`** rimette il lotto in servizio prima di `activation` (o alla fine del lotto) e **`restore-wait@…`** attende che il load balancer lo veda sano, fino a `restore_timeout`. Un nodo non torna mai in servizio senza essere sano.

Lo stato del rollout (`manent status`, interfaccia web) riporta per ogni passo i membri fuori dal load balancer (`drained`).

## Errori e rollback

Con il [rollback](rollback.md) attivo, `drain` e `restore` partecipano alle compensazioni in ordine inverso: un lotto già rimesso in servizio viene prima svuotato di nuovo, poi riportato alla release precedente, poi rimesso in servizio. Così il traffico torna solo su una release sana. Senza rollback, o con `onFailure: stop`, un nodo svuotato resta fuori e lo stato lo segnala.

In dry-run il pool non viene toccato.

## Provider

| Provider | Svuotamento | Vuoto quando | Rientro e salute |
|---|---|---|---|
| `haproxy` | Data Plane API su ogni istanza attiva: `admin_state: drain` | sessioni correnti (`scur`) a 0 su tutte le istanze | `admin_state: ready`; sano se `operational_state: up` ovunque |
| `aws-elb` | `DeregisterTargets` dal target group, o `EnterStandby` se indicato un Auto Scaling group | target non più in `draining` | `RegisterTargets` o `ExitStandby`; sano se `healthy` nel target group |
| `health-flag` | l'applicazione risponde 503 al proprio health check: chiamata HTTP o file flag via SSH | dopo `drain_delay` | flag tolto; sano se `health_url` risponde 200 |

### haproxy

| Impostazione | Descrizione |
|---|---|
| `dataplane_urls` | URL della Data Plane API di ogni istanza attiva, separati da virgola: lo stato cambia su tutte. |
| `backend` | Backend con i server. |
| `credentials_secret` | Segreto `utente:password` della Data Plane API. |
| `api_version` | `v3` (default) o `v2`. |
| `ca_file`, `allow_http` | CA dedicata; `"true"` per ammettere URL http (solo sviluppo). |

### aws-elb

| Impostazione | Descrizione |
|---|---|
| `region`, `target_group_arn` | Target group dei membri. Il membro è l'ID del target: l'ID dell'istanza (`member: "{label.instance_id}"`) o l'IP. |
| `port` | Porta dei target (default: quella del target, o quella del target group). |
| `auto_scaling_group` | Con un Auto Scaling group lo svuotamento usa lo standby, altrimenti l'ASG potrebbe sostituire l'istanza deregistrata. |
| `access_key_id_secret`, `secret_access_key_secret`, `role_arn`, … | Credenziali, come per i [plugin AWS](../plugins/ecs.md). |

Permessi minimi: `elasticloadbalancing:DescribeTargetHealth`, `RegisterTargets`, `DeregisterTargets` sul target group; con l'Auto Scaling group anche `autoscaling:DescribeAutoScalingInstances`, `EnterStandby`, `ExitStandby`.

### health-flag

Funziona con qualunque load balancer, ma non conosce gli altri membri del pool: il controllo di capacità non è disponibile e l'attesa è un tempo fisso.

| Impostazione | Descrizione |
|---|---|
| `mode` | `http` (default) o `file`. |
| `drain_url` | `http`: URL chiamato con `drain_method` (default `PUT`) per svuotare e con `restore_method` (default `DELETE`) per rientrare; `{host}`, `{port}` e `{label.<nome>}` vengono sostituiti. |
| `token_secret` | `http`: segreto con un bearer token. |
| `path` | `file`: file flag creato e rimosso via SSH (`ssh_user`, `ssh_key_secret` o `ssh_password_secret`, `known_hosts`). |
| `health_url` | Sonda della salute dopo il rientro (`http(s)://{host}…`). |
| `drain_delay` | Tempo che il load balancer impiega a vedere il target fuori servizio (default 30s). |

## Riferimento

| `traffic.pool` | Default | Descrizione |
|---|---|---|
| `provider` | | `haproxy`, `aws-elb`, `health-flag`. |
| `member` | `{host}` | Nome del membro di un target: `{host}`, `{port}`, `{label.<nome>}`. |
| `drain_timeout` | `5m` | Attesa massima della capacità e delle connessioni residue. |
| `restore_timeout` | `5m` | Attesa massima del rientro in salute. |
| `poll_interval` | `5s` | Frequenza delle letture durante le attese. |
| `min_healthy` | `1` | Membri sani da tenere in servizio: numero o percentuale dei membri del pool. |
| `on_timeout` | `proceed` | `proceed` o `fail`. |
| `settings` | | Impostazioni del provider; i segreti si indicano per nome. |
