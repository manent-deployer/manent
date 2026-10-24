# Slack

Notifiche di deploy e approvazioni nei canali Slack. Con un bot token il messaggio di avvio viene **aggiornato** con l'esito, così un canale mostra un messaggio per deploy; le richieste di approvazione hanno i pulsanti **Approva** e **Respingi**, che passano dagli stessi controlli dei permessi dell'API.

!!! abstract "Cosa serve"
    I diritti per creare un'app nel workspace Slack · un gestore dei segreti raggiungibile da manent (Vault negli esempi) · per i pulsanti di approvazione, l'indirizzo di manent raggiungibile da Slack in HTTPS

## 1. Crea l'app Slack

1. Su [api.slack.com/apps](https://api.slack.com/apps) scegli **Create New App → From scratch**, chiamala `manent` e scegli il workspace.
2. In **OAuth & Permissions → Bot Token Scopes** aggiungi `chat:write`. Aggiungi anche `users:read.email` se collegherai gli utenti Slack alle identità di manent tramite email (passo 5).
3. **Install to Workspace** e copia il **Bot User OAuth Token** (`xoxb-…`).
4. In **Basic Information → App Credentials** copia il **Signing Secret**: manent lo usa per verificare che i clic sui pulsanti arrivino davvero da Slack.
5. Invita il bot in ogni canale in cui scriverà: `/invite @manent` nel canale.

Salva entrambi i valori nel gestore dei segreti:

```sh
vault kv put secret/manent/slack bot_token=xoxb-… signing_secret=…
```

## 2. Configura le notifiche

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com      # i messaggi rimandano alla pagina del deploy
  language: it                                # en (default) o it
  slack:
    bot_token_ref: "vault:secret/data/manent/slack#bot_token"
    signing_secret_ref: "vault:secret/data/manent/slack#signing_secret"
  notifications:
    - name: prod
      applications: ["*"]
      environments: [prod]
      slack: {channel: C0123456789}           # ID del canale: dettagli del canale → Informazioni
    - name: payments-team
      applications: ["pay-*"]
      environments: ["*"]
      events: [deployment]                    # solo avvio ed esito, niente approvazioni
      slack: {channel: C0456789012}
```

- Ogni voce è una **destinazione** per un insieme di applicazioni e ambienti; un deploy va a tutte le destinazioni che gli corrispondono.
- `events` è `deployment` (avvio ed esito) e/o `approval` (richiesta e decisione). Default: entrambi.
- I dry-run non vengono notificati, salvo che la destinazione abbia `dry_run: true`.

Riavvia manent. Le destinazioni vengono fissate quando un deploy viene accodato: i deploy già in coda e le schedule ricorrenti esistenti mantengono quelle con cui sono stati creati.

!!! tip "Senza bot? Un incoming webhook"
    `slack: {webhook_ref: "vault:…#webhook_url"}` al posto di `channel` pubblica un messaggio per evento su un incoming webhook. I messaggi non vengono aggiornati, e i pulsanti di approvazione non sono disponibili.

## 3. Verifica

Aggiungi una destinazione temporanea che notifica anche i dry-run, ed eseguine uno:

```yaml
    - name: slack-test
      applications: [hello]
      environments: [test]
      dry_run: true
      slack: {channel: C0123456789}
```

```sh
manent deploy -f deploy/test.yml --dry-run
manent steps <id> | jq -r '.[] | select(.name | startswith("notify:")) | "\(.name) \(.error // "ok")"'
```

Ogni invio è uno step DBOS con nome `notify:<destinazione>:<evento>`. Un invio fallito riporta lì il suo errore e nel log del worker (`chat notification not sent`, inviato anche al SIEM), ma non fa mai fallire il deploy.

## 4. Approvazioni da Slack

Con `signing_secret_ref` configurato, manent registra `POST /integrations/slack/interactions`. Nell'app Slack:

1. **Interactivity & Shortcuts → On**.
2. **Request URL:** `https://manent.example.com/integrations/slack/interactions`.

Quando un deploy richiede [approvazioni](../guida/governance.md#approvazioni), le destinazioni con l'evento `approval` ricevono un messaggio con **Approva** e **Respingi**. Un clic:

1. viene verificato con il signing secret e rifiutato se più vecchio di 5 minuti;
2. viene attribuito a un'**identità di manent** (passo successivo);
3. viene registrato come decisione di quell'identità, con le stesse regole di `manent approve`: il permesso `deployment.approve` e il divieto di approvare il proprio deploy;
4. riceve una risposta visibile solo a chi ha cliccato: decisione registrata, o perché è stata rifiutata.

Quando l'approvazione termina (approvata, rifiutata o scaduta) il messaggio viene aggiornato: pulsanti rimossi, decisioni elencate.

!!! note "Raggiungibilità"
    Slack chiama la Request URL da Internet. Se manent è interno, pubblica solo questo percorso tramite il reverse proxy o l'API gateway; tutto il resto può restare privato.

## 5. Collega gli utenti Slack alle identità

Un utente Slack non collegato a un'identità non può decidere nulla. Due modi:

=== "Elenco esplicito"

    ```yaml
    integrations:
      slack:
        identities:
          U024BE7LH: {subject: "oidc:alice", groups: [cab]}
          U031XQ7PZ: {subject: "oidc:bob", groups: [cab]}
    ```

    Il member ID di Slack si trova nel profilo dell'utente (**⋮ → Copy member ID**). I `groups` sono quelli usati dai binding dell'RBAC.

=== "Per email"

    ```yaml
    integrations:
      slack:
        email_identity: "oidc:{email}"     # richiede lo scope users:read.email
    ```

    L'email del profilo Slack diventa `oidc:alice@example.com`. Usalo solo se le email di Slack sono quelle dell'identity provider, e se il tuo `username_claim` produce email.

!!! warning "Il collegamento fa parte della sicurezza"
    Chi è collegato a `oidc:alice` approva come Alice. Tieni `identities` nella configurazione gestita dal team di piattaforma, e rivedila come una lista di accesso.

## Risoluzione dei problemi

| Sintomo | Verifica |
|---|---|
| Nessun messaggio | `applications`/`environments` della destinazione corrispondono al deploy; il bot è membro del canale; l'errore dello step `notify:` in `manent steps`. |
| Un nuovo messaggio per evento invece di un aggiornamento | La destinazione usa `webhook_ref`, oppure manca il bot token. |
| I pulsanti non fanno nulla | Interactivity è attiva, la Request URL è raggiungibile da Slack, `signing_secret_ref` è impostato (altrimenti l'endpoint non viene registrato). |
| "Non collegato" o permesso negato | L'utente è in `identities` (o `email_identity` si risolve), e quell'identità ha `deployment.approve` sull'applicazione e sull'ambiente. |

Riferimento: [Chat e change management](../guida/integrazioni.md#notifiche).
