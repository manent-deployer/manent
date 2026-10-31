# Callback

I callback notificano a sistemi esterni (pipeline CI, chat, sistemi di change management) l'esito di ogni step e quello finale del deploy. Si dichiarano nel descrittore: fino a 5 endpoint, che ricevono gli eventi in `POST` con corpo JSON.

```yaml
secrets:
  - name: callback-key
    ref: env:MANENT_CALLBACK_KEY

callbacks:
  - url: https://ci.example.com/hooks/manent
    auth:
      type: hmac
      secret: callback-key
  - url: https://chat.example.com/hooks/deploy
    events: [deployment]      # solo avvio ed esito finale
```

| Campo | Descrizione |
|---|---|
| `url` | URL assoluto `https` (o `http`, se ammesso dalla configurazione). Non può contenere credenziali: vanno passate con `auth`. |
| `events` | Tipi di evento da ricevere: `step`, `deployment`. Default: tutti. |
| `auth` | Autenticazione, opzionale. Vedi sotto. |

## Eventi

Un deploy produce gli eventi in questo ordine:

| # | `event` | `step` | `status` |
|---|---|---|---|
| 1 | `deployment` | | `started` |
| 2 | `step` | `config` | `succeeded` / `failed` |
| … | `step` | ogni step del piano | `succeeded` / `failed` / `skipped` (solo in dry-run) |
| … | `step` | `checkpoint:<step>`, `rollback:<step>` | `succeeded` / `failed`, solo con il [rollback](rollback.md) attivo |
| ultimo | `deployment` | | `succeeded` / `failed` |

L'evento finale viene inviato dopo la pulizia della directory di lavoro. Se uno step fallisce, gli step successivi non vengono eseguiti e quindi non generano eventi: dopo lo step fallito arrivano gli eventi delle eventuali compensazioni (`rollback:<step>`) e poi l'evento finale.

```json title="Evento di tipo step"
{
  "id": "myapp-1.2.3-test:4",
  "event": "step",
  "sequence": 4,
  "deployment_id": "myapp-1.2.3-test",
  "application": "myapp",
  "environment": "test",
  "step": "artifacts",
  "status": "succeeded",
  "output": {"local_path": "/work/myapp-1.2.3-test/myapp-1.2.3.tar.gz", "sha256": "9f86…", "size": "10240"},
  "timestamp": "2026-10-09T10:00:03Z"
}
```

```json title="Evento finale con errore"
{
  "id": "myapp-1.2.3-test:7",
  "event": "deployment",
  "sequence": 7,
  "deployment_id": "myapp-1.2.3-test",
  "application": "myapp",
  "environment": "test",
  "status": "failed",
  "error": "step \"deploy\": target 192.168.1.10: connection: …",
  "timestamp": "2026-10-09T10:00:41Z"
}
```

| Campo | Descrizione |
|---|---|
| `id` | `<deployment_id>:<sequence>`, chiave di deduplica dell'evento. |
| `sequence` | Numero progressivo dell'evento nel deploy. |
| `dry_run` | Presente e `true` nei [dry-run](dry-run.md). |
| `requested_by` | Identità che ha richiesto il deploy (es. `oidc:alice`), se nota. |
| `output` | Output dello step. Non contiene mai segreti. |
| `error` | Messaggio d'errore, se lo step o il deploy è fallito. |
| `rollback` | Solo nell'evento finale di un deploy fallito per cui la policy prevedeva il rollback: `completed`, `skipped` (nulla da ripristinare) o `failed`. |

## Header

| Header | Valore |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `manent` |
| `X-Manent-Event` | `step` oppure `deployment` |
| `X-Manent-Delivery` | L'`id` dell'evento |
| `X-Manent-Timestamp`, `X-Manent-Signature` | Solo con autenticazione `hmac` |

## Autenticazione

`auth.secret` è il **nome** di un segreto della sezione `secrets`, mai il valore: il descrittore viene salvato nel database, mentre il segreto viene letto solo al momento dell'invio. Il segreto deve essere dichiarato nella sezione `secrets` del descrittore stesso: l'URL del callback lo sceglie chi richiede il deploy, quindi i segreti della configurazione di ambiente (chiavi SSH, token dei cluster) non possono mai essergli inviati. Un descrittore che indica un altro segreto viene rifiutato.

| `type` | Campi aggiuntivi | Effetto |
|---|---|---|
| `bearer` | | `Authorization: Bearer <segreto>` |
| `basic` | `username` | `Authorization: Basic <username:segreto>` |
| `header` | `header` | `<header>: <segreto>`, es. `X-Api-Key` |
| `hmac` | | Firma del corpo, vedi sotto |

### Verificare la firma HMAC

Con `hmac` la richiesta porta:

```
X-Manent-Timestamp: 1760004003
X-Manent-Signature: sha256=<hex>
```

dove `<hex>` è l'HMAC-SHA256, calcolato con il segreto, della stringa `<timestamp>.<corpo>`. Il destinatario ricalcola la firma sul corpo ricevuto, la confronta in tempo costante e rifiuta le richieste con un timestamp troppo vecchio, per evitare che vengano ripetute.

```python title="Esempio di verifica (Python)"
import hashlib, hmac, time

def verifica(secret: bytes, headers, body: bytes, tolleranza=300) -> bool:
    ts = headers["X-Manent-Timestamp"]
    if abs(time.time() - int(ts)) > tolleranza:
        return False
    atteso = hmac.new(secret, ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    ricevuto = headers["X-Manent-Signature"].removeprefix("sha256=")
    return hmac.compare_digest(atteso, ricevuto)
```

## Consegna

- **Durabilità:** ogni invio è uno step DBOS del workflow. Dopo una recovery gli invii già completati non vengono ripetuti.
- **Duplicati:** un invio interrotto da un crash può arrivare due volte. Il destinatario deduplica sull'`id` dell'evento.
- **Retry:** una risposta diversa da `2xx`, un errore di rete o un timeout fanno ritentare l'invio fino a `callbacks.max_retries` volte. I redirect non vengono seguiti e contano come errore.
- **Errori:** un callback non consegnato viene registrato nel log ma **non fa fallire il deploy**.
- **Tempi:** gli invii avvengono in sequenza dentro il workflow. Un endpoint lento rallenta il deploy, al massimo di `timeout` per ogni tentativo.
- **Annullamento:** un deploy annullato con `cancel` non invia l'evento finale.

## Limiti sugli URL

Gli URL dei callback arrivano da chi richiede il deploy: senza limiti, un descrittore potrebbe far chiamare al worker servizi della rete interna. La sezione `callbacks` di `manent.yml` li restringe:

```yaml
callbacks:
  timeout: 10s
  max_retries: 3
  allow_http: false                 # di default solo https
  allowed_hosts:                    # vuoto = tutti gli host
    - ci.example.com
    - "*.hooks.example.com"
```

Un URL fuori policy viene rifiutato già all'accodamento (`422` dall'API) e controllato di nuovo prima di ogni invio.
