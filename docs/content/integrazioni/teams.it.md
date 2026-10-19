# Microsoft Teams

Notifiche di deploy e approvazioni nei canali Teams, come Adaptive Card. I webhook di Teams non possono ricevere clic, quindi le card di approvazione hanno un pulsante **Valuta in manent** che apre il deploy nell'[interfaccia web](../guida/interfaccia-web.md), dove l'utente accede e decide con le regole abituali.

!!! abstract "Cosa serve"
    Un canale Teams in cui puoi aggiungere un workflow · un gestore dei segreti raggiungibile da manent · `integrations.public_url` impostato, così le card possono rimandare all'interfaccia web

## 1. Crea il webhook

Con l'app **Workflows** (che sostituisce i connettori di Office 365):

1. Nel canale, **⋯ → Workflows**, e scegli il modello **Post to a channel when a webhook request is received**.
2. Scegli team e canale, e crea il workflow.
3. Copia l'URL che Teams mostra alla fine. Chiunque abbia questo URL può scrivere nel canale: trattalo come un segreto.

Un classico *Incoming Webhook* del canale, dove ancora disponibile, funziona allo stesso modo.

```sh
vault kv put secret/manent/teams cab_webhook='https://…'
```

## 2. Configura la destinazione

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com
  language: it
  notifications:
    - name: cab
      applications: ["*"]
      environments: ["prod*"]
      events: [approval]                       # il change advisory board vede solo le approvazioni
      teams: {webhook_ref: "vault:secret/data/manent/teams#cab_webhook"}
    - name: ops
      applications: ["*"]
      environments: [prod]
      events: [deployment]
      teams: {webhook_ref: "vault:secret/data/manent/teams#ops_webhook"}
```

L'URL del webhook viene letto dal gestore dei segreti a ogni invio, quindi si può ruotare senza riavviare manent. Le destinazioni vengono fissate quando un deploy viene accodato.

## 3. Cosa mostrano le card

| Evento | Card |
|---|---|
| `deployment` avviato | Applicazione, ambiente, versione, richiedente, eventuale ticket di change, link al deploy. |
| `deployment` concluso | L'esito; per gli errori l'errore e l'esito del rollback automatico. |
| `approval` richiesta | Chi ha chiesto, cosa, quante approvazioni servono e fino a quando, con **Valuta in manent**. |
| `approval` conclusa | Approvata, rifiutata o scaduta, con le decisioni. |

Ogni evento è una nuova card: a differenza di Slack con un bot token, un webhook non può aggiornare un messaggio già pubblicato.

## 4. Verifica

Aggiungi `dry_run: true` a una destinazione per un ambiente di test ed esegui `manent deploy --dry-run`. L'invio è lo step `notify:<destinazione>:<evento>` in `manent steps <id>`; un errore viene registrato lì e nel log del worker, e non fa mai fallire il deploy.

!!! note "Pulsanti interattivi in Teams"
    Approvare direttamente dentro Teams richiederebbe un bot registrato in Azure. manent tiene la decisione nell'interfaccia web, che applica già accesso, permessi e divieto di auto-approvazione.

Riferimento: [Chat e change management](../guida/integrazioni.md#notifiche).
