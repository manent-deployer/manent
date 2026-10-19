# Chat e change management

manent si integra con gli strumenti in cui i rilasci vengono discussi e approvati:

- **notifiche** in Slack e Microsoft Teams: avvio ed esito dei deploy, richieste e decisioni di approvazione;
- **approvazioni da Slack**, con i pulsanti Approva e Respingi, che rispettano gli stessi permessi dell'API;
- **ticket di change** in ServiceNow o Jira: aperti all'avvio del deploy e chiusi con il suo esito, con l'eventuale attesa dell'approvazione del change prima dell'esecuzione.

Tutto si configura in `manent.yml` sotto `integrations`; il change management si attiva per ambiente nelle [regole di governance](governance.md). Come per i [callback](callback.md), ogni invio è uno step DBOS: un deploy ripreso dopo un crash non perde né duplica i messaggi, e una notifica fallita non fa mai fallire il deploy.

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com   # link all'interfaccia web nei messaggi e nei ticket
  language: it                             # en (default) o it
  slack:
    bot_token_ref: "vault:secret/data/manent/slack#bot_token"
    signing_secret_ref: "vault:secret/data/manent/slack#signing_secret"
    identities:
      U024BE7LH: {subject: "oidc:alice", groups: [cab]}
    # email_identity: "oidc:{email}"
  notifications:
    - name: prod
      applications: ["*"]
      environments: [prod]
      slack: {channel: C0123456789}
    - name: cab
      applications: ["*"]
      environments: ["prod*"]
      events: [approval]
      teams: {webhook_ref: "vault:secret/data/manent/teams#cab_webhook"}
  change:
    provider: servicenow
    servicenow:
      url: https://acme.service-now.com
      username: manent
      password_ref: "vault:secret/data/manent/servicenow#password"
      fields: {assignment_group: "Release Management", cmdb_ci: "{application}"}
governance:
  rules:
    - name: prod
      applications: ["*"]
      environments: [prod]
      approvals: {required: 1}
      change: {approval: true, timeout: 72h}
```

## Notifiche { #notifiche }

Ogni voce di `integrations.notifications` è una destinazione per un insieme di applicazioni e ambienti (pattern con `*`). Un deploy viene inviato a **tutte** le destinazioni che gli corrispondono.

| Chiave | Descrizione |
|---|---|
| `name` | Minuscole, cifre e `-`. Compare nei nomi degli step DBOS (`notify:<nome>:<evento>`). |
| `applications`, `environments` | Obbligatori, `"*"` per tutti. |
| `events` | `deployment` (avvio ed esito) e/o `approval` (richiesta ed esito). Default entrambi. |
| `dry_run` | `true` per notificare anche i dry-run (default no). |
| `slack.channel` | ID o nome del canale, con il bot token: il messaggio di avvio viene **aggiornato** con l'esito e le richieste di approvazione hanno i pulsanti. |
| `slack.webhook_ref` | Al posto del canale: segreto con l'URL di un incoming webhook. Un messaggio per evento, senza pulsanti. |
| `teams.webhook_ref` | Segreto con l'URL di un webhook di Teams: un flusso dell'app Workflows ("Pubblica in un canale quando viene ricevuta una richiesta webhook") o un webhook del canale. I messaggi sono Adaptive Card. |

I messaggi riportano applicazione, ambiente, versione, chi ha richiesto il deploy, il ticket di change e, per i fallimenti, l'errore e l'esito del rollback automatico. Con `public_url` portano alla pagina del deploy nell'[interfaccia web](interfaccia-web.md).

Le destinazioni di un deploy si scelgono all'accodamento e restano salvate con la richiesta, come il [piano](../concetti/pipeline.md): cambiare `integrations.notifications` non altera i deploy già accodati né le [schedule ricorrenti](pianificazione.md) già create. Una destinazione rimossa dalla configurazione smette semplicemente di ricevere messaggi.

### App Slack

1. Crea un'app su [api.slack.com/apps](https://api.slack.com/apps) con lo scope del bot `chat:write` (e `users:read.email` se usi `email_identity`), installala nel workspace e invita il bot nei canali.
2. Salva il **Bot User OAuth Token** (`xoxb-…`) e il **Signing Secret** nel gestore dei segreti e riferiscili in `bot_token_ref` e `signing_secret_ref`.
3. Per le approvazioni, attiva **Interactivity** e imposta come Request URL `https://<manent>/integrations/slack/interactions`. L'indirizzo deve essere raggiungibile da Slack.

## Approvazioni da chat { #approvazioni-da-chat }

Quando un deploy richiede [approvazioni](governance.md#approvazioni), la richiesta arriva alle destinazioni con eventi `approval`.

**Slack.** Il messaggio ha i pulsanti **Approva** e **Respingi**. Un clic arriva a `/integrations/slack/interactions`, che:

1. verifica la firma di Slack (`X-Slack-Signature`, HMAC con il signing secret) e rifiuta le richieste più vecchie di 5 minuti;
2. trova l'**identità di manent** dell'utente: prima in `slack.identities` (ID utente Slack → `subject` e `groups`), altrimenti, con `email_identity`, dall'email del profilo Slack (`oidc:{email}` diventa `oidc:alice@example.com`);
3. registra la decisione **come quell'identità**, con gli stessi controlli dei permessi dell'API: il permesso `deployment.approve` su applicazione e ambiente e il divieto di approvare il proprio deploy;
4. risponde all'utente con un messaggio visibile solo a lui: decisione registrata, o il motivo per cui è stata rifiutata.

Il messaggio del canale viene aggiornato alla fine dell'approvazione (approvata, respinta o scaduta): i pulsanti spariscono e le decisioni vengono elencate. Un utente non associato a un'identità non può decidere nulla.

!!! warning "L'associazione fa parte della sicurezza"
    Chi è associato a `oidc:alice` in `slack.identities` approva come Alice. Tieni l'elenco nella configurazione gestita dagli operatori, e usa `email_identity` solo se le email di Slack sono quelle del tuo identity provider. Senza signing secret l'endpoint non viene registrato.

**Teams.** I webhook di Teams non ricevono i clic: la card ha un pulsante **Valuta in manent** che apre il deploy nell'interfaccia web, dove l'utente accede e decide con le stesse regole. Pulsanti interattivi in Teams richiederebbero un bot registrato in Azure.

## Change management { #change-management }

Con `change` in una regola di governance, ogni deploy reale di quelle applicazioni e ambienti (non i dry-run) ha un ticket di change:

1. **Apertura.** All'avvio del deploy lo step `change:open` cerca il ticket del deploy e lo crea se non esiste, poi lo porta in implementazione. Il ticket si ritrova dall'ID del deploy (`correlation_id` in ServiceNow, l'etichetta `manent-<id>` in Jira), quindi retry e recovery non ne creano mai un secondo. **Senza ticket il deploy non parte.**
2. **Chiusura.** Alla fine lo step `change:close` registra l'esito (ed eventualmente il rollback) e chiude il ticket. Un errore in questa fase viene registrato nel log: il deploy è già concluso.

Numero e link del ticket compaiono nello stato del deploy (`change`), nella CLI, nell'interfaccia web e nei messaggi in chat.

### Attesa dell'approvazione del change

Con `change: {approval: true}` il deploy attende anche che il change sia **approvato nel sistema ITSM**:

- il ticket viene aperto alla richiesta del deploy, nel [workflow di approvazione](governance.md#approvazioni), che non occupa slot dei worker;
- manent ne legge lo stato a intervalli crescenti, da `integrations.change.poll_interval` (default 1 minuto) fino a 5 minuti;
- il deploy parte quando valgono **entrambe** le condizioni: le approvazioni di manent richieste da `approvals` (se presenti) e il change approvato. Un change respinto o annullato nell'ITSM respinge il deploy, con una decisione intestata al ticket (`servicenow:CHG0031337`);
- l'attesa scade dopo `change.timeout` (default 72 ore) o `approvals.timeout`, il più lungo dei due.

Un change già approvato alla creazione (ad esempio uno standard change pre-approvato) non fa attendere il deploy.

| `governance.rules[].change` | Default | Descrizione |
|---|---|---|
| `approval` | `false` | Attendere l'approvazione del change nel sistema ITSM. |
| `timeout` | `72h` | Attesa massima. |

### ServiceNow

La Table API su `change_request`, con autenticazione basic (`username`, `password_ref`) o un token OAuth (`token_ref`).

| `integrations.change.servicenow` | Default | Descrizione |
|---|---|---|
| `url` | | Indirizzo dell'istanza. |
| `table` | `change_request` | Tabella dei change. |
| `fields` | | Campi aggiuntivi del ticket (`type`, `assignment_group`, `cmdb_ci`, `category`…). I valori accettano `{application}`, `{environment}`, `{version}`, `{artifact}`, `{deployment}`, `{requested_by}`. `correlation_id` e `correlation_display` li imposta manent e qui vengono rifiutati. |
| `start_state` | `-1` (Implement) | Stato impostato all'avvio del deploy; `""` per lasciarlo invariato. |
| `close_states` | `["0", "3"]` (Review, Closed) | Stati impostati in ordine alla fine. |
| `close_codes` | `success: successful`, `failure: unsuccessful` | `close_code` per esito; `close_notes` lo descrive. |

L'approvazione si legge dal campo `approval` (`approved`, `rejected`); un change annullato (stato `4`) vale come respinto. Se il tuo modello di stato non ammette le transizioni, adatta `start_state` e `close_states`: un avvio rifiutato viene registrato e non blocca il deploy.

L'utente di manent deve poter leggere, creare e aggiornare i record della tabella (ad esempio il ruolo `itil`, o un ruolo dedicato con ACL su `change_request`).

### Jira

L'API REST v2, su Jira Cloud (`username` con l'email e un API token in `token_ref`) o Jira Data Center (solo `token_ref`, un personal access token).

| `integrations.change.jira` | Descrizione |
|---|---|
| `url`, `project`, `issue_type` | Dove creare l'issue (ad esempio progetto `CHG`, tipo `Change`). |
| `fields` | Campi aggiuntivi, con valori JSON e gli stessi segnaposto di ServiceNow (ad esempio `components: [{name: "{application}"}]`). `labels` (un elenco di stringhe) si aggiungono a `manent` e `manent-<id>` di manent; `project` e `issuetype` vengono rifiutati: usa `project` e `issue_type`. |
| `approved_statuses`, `rejected_statuses` | Stati che approvano o respingono il change. Obbligatori con `approval: true`. |
| `transitions` | Nomi delle transizioni: `start` (avvio del deploy), `success` e `failure` (fine). Le transizioni mancanti vengono saltate. |

Alla fine manent aggiunge un commento con l'esito ed esegue la transizione. Le approvazioni di Jira Service Management si leggono dallo stato dell'issue: il passo di approvazione del workflow porta l'issue in uno degli `approved_statuses`.

## Affidabilità

- Notifiche e operazioni sul change sono step DBOS con retry (`callbacks.max_retries` per le notifiche, 3 per i change). Gli errori 4xx dell'ITSM (salvo 408 e 429) sono permanenti e non vengono ritentati.
- Destinazioni e regole di change sono fissate nella richiesta: un cambio di configurazione durante una recovery non cambia la sequenza degli step dei workflow in corso.
- URL dei webhook e token si leggono dal gestore dei segreti a ogni uso e non compaiono mai in log, output o messaggi.
- I redirect non vengono seguiti, così le credenziali non arrivano mai a un altro host.
