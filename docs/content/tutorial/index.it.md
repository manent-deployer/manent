# Tutorial

Tutorial brevi e pratici, uno dopo l'altro. Ognuno riparte da dove è finito il precedente, usa solo comandi e chiavi di configurazione reali e termina con una verifica da fare da sé.

!!! tip "Vuoi solo vederlo girare?"
    L'[Avvio rapido](../avvio-rapido.md) compila manent ed esegue un deploy in cinque minuti. I tutorial spiegano cosa succede e perché.

## Percorso

<div class="grid cards" markdown>

-   :material-numeric-1-circle-outline:{ .lg .middle } **[Primo deploy su un server Linux](primo-deploy.md)**

    ---

    Rilascia un archivio via SSH con una directory per versione, simulalo con un dry-run, osserva un rollback automatico e torna indietro con uno manuale.

    *30 minuti · Docker, Go*

-   :material-numeric-2-circle-outline:{ .lg .middle } **[Ambienti e segreti](ambienti.md)**

    ---

    Sposta target e parametri dal descrittore a Consul, leggi la chiave SSH da Vault e limita ciò che un descrittore può chiedere.

    *30 minuti · prosegue il tutorial 1*

-   :material-numeric-3-circle-outline:{ .lg .middle } **[Promozione, approvazioni e freeze](promozione.md)**

    ---

    Attiva autenticazione e ruoli, fai in modo che la produzione riceva solo ciò che è passato da test, approva con una seconda persona e congela la produzione durante un incidente.

    *25 minuti · prosegue il tutorial 2*

-   :material-numeric-4-circle-outline:{ .lg .middle } **[Deploy su Kubernetes](kubernetes.md)**

    ---

    Rilascia manifest o un chart Helm su un cluster con una service account limitata a un namespace, segui il rollout e torna indietro con lo storico conservato nel cluster.

    *30 minuti · un cluster Kubernetes*

-   :material-numeric-5-circle-outline:{ .lg .middle } **[Rilascio progressivo con health gate](rilascio-progressivo.md)**

    ---

    Rilascia prima un canary, poi una AZ alla volta, con tempi di osservazione, sonde HTTP, un gate sul tasso di errori in Prometheus, un'approvazione dopo il canary e rollback automatico.

    *30 minuti · Prometheus*

-   :material-numeric-6-circle-outline:{ .lg .middle } **[Deploy da una pipeline di CI](ci.md)**

    ---

    Dai alla CI un token e un ruolo dedicati, rilascia da GitHub Actions o GitLab CI con un job idempotente, pubblica l'esito e verifica le modifiche alla configurazione.

    *20 minuti · GitHub o GitLab*

-   :material-numeric-7-circle-outline:{ .lg .middle } **[Infrastruttura e configurazione in catena](catene.md)**

    ---

    Un deploy dell'infrastruttura crea le VM ed esporta i loro indirizzi, un deploy della configurazione li importa come target: in catena con `--import`, poi dalla release in esercizio, con un export sbagliato che ferma la catena.

    *25 minuti · prosegue il tutorial 1*

-   :material-numeric-8-circle-outline:{ .lg .middle } **[manent in produzione](produzione.md)**

    ---

    Postgres, più worker, accesso OIDC, TLS, monitoraggio, SIEM e backup: la checklist per passare dal portatile a un servizio di piattaforma.

    *45 minuti · Kubernetes o VM*

</div>

## Integrazioni

Una volta presa confidenza, la sezione [Integrazioni](../integrazioni/index.md) ha una guida passo passo per ogni strumento esterno: Slack, Microsoft Teams, ServiceNow, Jira, GitHub Actions, GitLab CI, Terraform, Vault, Consul, identity provider, Prometheus e gli altri sistemi di metriche, repository di artefatti e SIEM.

## Convenzioni

- I comandi si eseguono dalla directory del tutorial, con `manent` e `manent-server` nel `PATH`.
- Output dei comandi e log sono riportati come li stampa manent: in inglese.
- I valori da sostituire sono tra parentesi angolari, per esempio `<tuo-cluster>`.
