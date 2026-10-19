# Glossario

**Activation**
:   L'ultimo step standard (`activation`): rende operativa una release quando copiarla non basta. Facoltativo, e compensabile se l'activator lo supporta.

**Adozione**
:   Un deploy di tipo `adoption` che registra nell'inventario una release già in esercizio senza toccare i target. Serve per passare da un ambiente per cliente ai [tenant](../guida/tenant.md).

**After (dipendenza)**
:   Un deploy con `after` attende in `WAITING_DEPENDENCY` che i deploy elencati riescano. Vedi [Catene di deploy](../guida/catene.md).

**Ambiente**
:   Dove gira un'applicazione (`test`, `prod`). Region e AZ sono etichette dei target dentro un solo ambiente.

**Approvazione**
:   Una decisione di un'identità diversa dal richiedente, richiesta da una regola di governance prima di un deploy o tra le ondate. Vedi [Governance dei rilasci](../guida/governance.md#approvazioni).

**Artefatto**
:   Il file o l'immagine immutabile che un deploy rilascia, identificato da `type`, `name`, `version`, `source` e dal suo sha256.

**Bake**
:   Il tempo di osservazione dopo uno stadio del rollout o un gradino di traffico, durante il quale l'health gate viene letto più volte.

**Callback**
:   Un endpoint HTTP che riceve un evento per ogni step e per l'esito di un deploy. Vedi [Callback](../guida/callback.md).

**Canary**
:   Una prima piccola quota del rilascio, pochi nodi in un rollout a ondate o pochi punti percentuali di traffico, osservata prima di proseguire.

**Candidata, stabile**
:   In un [rilascio per percentuale](../guida/traffico.md), la nuova versione in rilascio e quella in esercizio.

**Checkpoint**
:   Uno step DBOS (`checkpoint:<step>`) che registra, prima di uno step compensabile, lo stato a cui tornare in un rollback.

**Chiave di idempotenza**
:   L'`id` del descrittore. Una seconda richiesta con lo stesso `id` restituisce il deploy esistente invece di eseguirlo di nuovo.

**Compensazione**
:   L'annullamento degli effetti di uno step durante un rollback (`rollback:<step>`).

**Configurazione di ambiente**
:   Documento per applicazione e ambiente con plugin, target, parametri, riferimenti ai segreti e strategia, letto da Consul o da file. Vedi [Configurazione di ambiente](../guida/configurazione-ambiente.md).

**DBOS**
:   La libreria di workflow durevoli su cui si basa manent. Registra l'input di ogni workflow e il risultato di ogni step in Postgres (o SQLite in sviluppo).

**Deploy**
:   Un'esecuzione della pipeline per un'applicazione, un ambiente e un artefatto. Ha un ID, uno stato, un tipo e un esito.

**Deploy effettivo**
:   Il descrittore fuso con la configurazione di ambiente (e con il tenant): ciò che i plugin ricevono davvero. Lo mostra `manent validate`.

**Descrittore**
:   Il documento YAML (CLI) o JSON (API) con cui si chiede un deploy. Vedi [Descrittore di deploy](../guida/descrittore.md).

**Drift**
:   Un ambiente in cui non tutti i target eseguono la versione rilasciata da manent: dopo un rollout fermato a metà (`drift`) o dopo modifiche fuori da manent (`out_of_sync`).

**Dry-run**
:   Un deploy eseguito come simulazione: stesso workflow, nessuna modifica ai target. Vedi [Dry-run](../guida/dry-run.md).

**Executor ID**
:   L'identificativo stabile di un worker (`dbos.executor_id`, default l'hostname). Dopo un riavvio un worker riprende solo i workflow con il proprio ID.

**Finestra di freeze**
:   Un periodo in cui i deploy reali sono bloccati, configurato o creato a runtime. Le finestre `hard` non ammettono deroghe. Vedi [Governance dei rilasci](../guida/governance.md#finestre-di-freeze).

**Gate (health gate)**
:   La verifica dopo uno stadio del rollout o un gradino di traffico: sonde HTTP, query sulle metriche e allarmi. Se è rosso il rollout si ferma.

**Governance**
:   Le regole su cosa si può rilasciare, dove e quando: promozione obbligatoria, approvazioni, freeze, ticket di change. Scritte in YAML o in [Rego](../guida/policy.md).

**Inventario**
:   La release in esercizio per applicazione, ambiente (e tenant), ricostruita dallo storico dei deploy. Vedi [Governance dei rilasci](../guida/governance.md#inventario).

**Locator**
:   Il modo in cui si trova la release a cui tornare in un rollback; `plugin` la legge dai target.

**Lotto**
:   Dentro un passo del rollout, il gruppo di al più `maxUnavailable` target aggiornati insieme.

**Ondata**
:   Un gruppo di target selezionati per etichetta in un rollout a ondate, rilasciato dopo che il gate dell'ondata precedente è verde.

**Passo**
:   Il lavoro su una region o una AZ dentro uno stadio del rollout, con nome `<ondata>/<region>/<az>`.

**Piano**
:   L'elenco ordinato degli step di un deploy, calcolato all'invio da `pipeline.steps` e dalla sezione `steps` del descrittore.

**Plugin**
:   Il componente che rilascia un tipo di artefatto su un tipo di target: incorporato oppure un processo [esterno](../plugins/esterni.md) via gRPC.

**Policy sui descrittori**
:   La sezione `descriptor_policy` di `manent.yml`: cosa un descrittore può aggiungere alla configurazione di ambiente (parametri, riferimenti ai segreti, target).

**Promozione**
:   Rilasciare in un ambiente esattamente l'artefatto (stesso sha256) già rilasciato con successo in un altro.

**Recovery**
:   La ripresa dei workflow di un worker che si è fermato, sullo stesso worker dopo un riavvio o su un altro.

**Region routing**
:   Con `dbos.region_routing`, ogni passo del rollout gira su un worker della region del passo.

**Release composta**
:   Un documento `kind: Release` che raggruppa più deploy con dipendenze e una policy `onFailure`. Vedi [Catene di deploy](../guida/catene.md#release-composte).

**Riconciliazione**
:   La lettura periodica della release effettivamente in esercizio sui target, per segnalare quelli cambiati fuori da manent. Vedi [Stato reale dei target](../guida/governance.md#stato-reale).

**Riferimento a un segreto**
:   Un puntatore a un segreto (`vault:…`, `file:…`, `env:…`), mai il suo valore. Vedi [Segreti e sorgenti degli artefatti](../operazioni/segreti-artefatti.md).

**Rollback**
:   Il ritorno alla release precedente: automatico dentro un deploy fallito, o manuale come nuovo deploy di tipo `rollback`. Vedi [Rollback](../guida/rollback.md).

**Rollout**
:   La strategia che divide un deploy in stadi: [ondate](../guida/ondate.md) o [percentuali di traffico](../guida/traffico.md).

**Schedule**
:   Un deploy ricorrente creato da un descrittore con `schedule.cron`. Ogni scatto accoda un deploy `<schedule>@<ora UTC>`.

**Slot**
:   Nei rilasci per percentuale, uno dei due posti (`a`, `b`, oppure produzione e uno slot di staging) in cui candidata e stabile girano affiancate.

**Stadio**
:   Un gruppo di passi del rollout eseguiti insieme e seguiti dallo stesso bake e dallo stesso gate.

**Step**
:   Una fase della pipeline, eseguita come step DBOS con i propri tentativi e un risultato registrato. Step standard: `secrets`, `artifacts`, `qa`, `deploy`, `post-test`, `activation`; step personalizzati di tipo `command` o `http`.

**Step `config`**
:   Il primo step di ogni deploy: legge la configurazione di ambiente, la fonde con il descrittore e ricontrolla freeze e policy.

**Svuotamento**
:   Togliere un lotto di target dal load balancer prima del deploy e rimetterlo in servizio dopo. Vedi [Svuotamento dal load balancer](../guida/svuotamento.md).

**Target**
:   Una destinazione del deploy: un host, un cluster, una region o un servizio cloud, un endpoint di management. Ha delle etichette.

**Tenant**
:   Un cliente o un'istanza con parametri, segreti e target propri dentro un ambiente condiviso. Vedi [Tenant](../guida/tenant.md).

**Tipo (kind)**
:   Come è nato un deploy: `deploy`, `promotion`, `rollback` o `adoption`.

**Tipo di artefatto**
:   Il campo `artifact.type` (`application`, `container`, `kubernetes`, `war`, `migration`…). Sceglie i plugin in grado di gestire l'artefatto e la policy di rollback.

**Worker**
:   Un processo `manent-server` (`serve` o `worker`) che esegue i deploy dalla coda.
