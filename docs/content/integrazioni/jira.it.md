# Jira

Una issue Jira per ogni deploy reale delle applicazioni e degli ambienti che scegli, su Jira Cloud o Jira Data Center: creata all'avvio del deploy, portata avanti con le transizioni del tuo workflow e commentata con l'esito alla fine. Facoltativamente il deploy **attende** che la issue raggiunga uno stato approvato, che è il modo in cui si rispettano le approvazioni di Jira Service Management.

!!! abstract "Cosa serve"
    Un progetto Jira per i change (per esempio `CHG` con un tipo di issue `Change`) · un account tecnico · un gestore dei segreti raggiungibile da manent

## 1. Prepara Jira

1. Scegli o crea il progetto e il tipo di issue che useranno i change.
2. Guarda il workflow di quel tipo di issue e annota i **nomi** di:
    - la transizione da eseguire all'avvio del deploy (per esempio `Implement`);
    - le transizioni per successo ed errore alla fine (`Done`, `Failed`);
    - con le approvazioni, gli stati che significano approvato e respinto (`Approved`, `Declined`).
3. Crea le credenziali:
    - **Jira Cloud:** un account (la sua email è lo `username`) e un [API token](https://id.atlassian.com/manage-profile/security/api-tokens);
    - **Jira Data Center:** un personal access token, senza `username`.
4. Dai all'account i permessi per sfogliare, creare, modificare, commentare e far avanzare le issue del progetto.

```sh
vault kv put secret/manent/jira token='…'
```

## 2. Configura il provider

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com
  change:
    provider: jira
    jira:
      url: https://acme.atlassian.net
      username: manent@acme.com                 # solo Jira Cloud
      token_ref: "vault:secret/data/manent/jira#token"
      project: CHG
      issue_type: Change
      fields:
        components: [{name: "{application}"}]
        priority: {name: Medium}
      transitions: {start: Implement, success: Done, failure: Failed}
      approved_statuses: [Approved]
      rejected_statuses: [Declined]
```

- `fields` sono valori JSON con i segnaposto `{application}`, `{environment}`, `{version}`, `{artifact}`, `{deployment}`, `{requested_by}`. manent imposta già `project`, `issuetype`, `summary`, `description` e `labels`; una chiave in `fields` sostituisce il valore di manent.
- Non impostare `labels` in `fields`: manent ritrova la issue tramite la sua etichetta `manent-<id del deploy>`.
- Una transizione assente dal workflow viene saltata.
- `approved_statuses` e `rejected_statuses` sono obbligatori con `change.approval: true`.

## 3. Attivalo per ambiente

```yaml title="manent.yml"
governance:
  rules:
    - name: production
      applications: ["*"]
      environments: [prod]
      change: {approval: true, timeout: 72h}
```

Senza `approval` la issue viene creata e chiusa attorno al deploy. Con `approval: true` viene creata quando il deploy viene richiesto, e il deploy attende che la issue raggiunga uno degli `approved_statuses` (uno stato in `rejected_statuses` lo respinge). In Jira Service Management, fai in modo che lo step di approvazione del workflow porti la issue nello stato approvato.

## 4. Verifica

```sh
manent deploy -f deploy/prod.yml --no-wait
manent status <id> | jq .change
```

La issue ha l'etichetta `manent-<id del deploy>`: manent la cerca con quell'etichetta (tramite `/rest/api/2/search/jql`, o `/search` sui server più vecchi) dopo i tentativi o una recovery, così un deploy non crea mai due issue. Alla fine manent aggiunge un commento con l'esito, e con l'esito del rollback se c'è stato, poi applica la transizione `success` o `failure`.

## Comportamento e limiti

Gli stessi di [ServiceNow](servicenow.md#comportamento-e-limiti): niente ticket, niente deploy; gli errori di chiusura vengono solo registrati nel log; gli errori 4xx permanenti non vengono ritentati; i dry-run non creano mai issue.

Riferimento: [Chat e change management](../guida/integrazioni.md#change-management).
