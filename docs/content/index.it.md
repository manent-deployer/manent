---
title: manent
hide:
  - navigation
  - toc
---

<div class="mn-hero" markdown>

<div markdown>

<span class="mn-hero__eyebrow">deploy as code · workflow durevoli DBOS</span>

# manent

<p class="mn-hero__lead">Porta in esercizio un artefatto <strong>così com'è stato testato</strong>: una pipeline di step dichiarativa, durevole e verificabile, dal commit della CI fino all'ultimo target.</p>

<p class="mn-hero__lead">Il nome viene dal latino <em>manere</em>, "rimanere, restare saldo": l'artefatto non cambia lungo il viaggio.</p>

<div class="mn-hero__actions">
<a class="md-button md-button--primary" href="avvio-rapido/">Avvio rapido →</a>
<a class="md-button" href="concetti/architettura/">Architettura</a>
<a class="md-button" href="guida/descrittore/">Descrittore</a>
</div>

</div>

<div class="mn-terminal">
<div class="mn-terminal__bar"><i></i><i></i><i></i><span>~/myapp — manent</span></div>
<pre><code><span class="p">$</span> manent deploy -f release.yml
<span class="dim">deploy accodato: 7c0e… (manent logs -f 7c0e…)</span>
<span class="p">$</span> manent logs -f 7c0e…
<span class="dim">14:03:10</span> INFO  <span class="v">[secrets]</span>    segreti verificati
<span class="dim">14:03:12</span> INFO  <span class="v">[artifacts]</span>  sha256=2d27fbdf… verificato
<span class="dim">14:03:12</span> INFO  <span class="v">[qa]</span>         QA superata
<span class="dim">14:03:15</span> INFO  <span class="v">[deploy]</span>     release 1.4.0 attivata su 3 target
<span class="dim">14:03:15</span> INFO  <span class="v">[activation]</span> in esercizio
<span class="ok">✔ SUCCESS</span>  myapp/prod  1.4.0  <span class="dim">kind=promotion</span></code></pre>
</div>

</div>

<div class="mn-stats">
<div class="mn-stat"><b>Durevole</b><span>riprende dall'ultimo step</span></div>
<div class="mn-stat"><b>Idempotente</b><span>mai due volte lo stesso deploy</span></div>
<div class="mn-stat"><b>Governato</b><span>promozioni · approvazioni · freeze</span></div>
<div class="mn-stat"><b>Osservabile</b><span>metriche · tracce · log · DORA</span></div>
</div>

Il deploy si descrive in un file dichiarativo, il **descrittore**. Il client `manent` lo legge in YAML e lo invia all'API di `manent-server`, che lo accoda; un worker lo esegue come workflow durevole [DBOS](https://github.com/dbos-inc/dbos-transact-golang).

```yaml title="release.yml"
apiVersion: manent/v1
application: myapp
environment: test
artifact:
  type: application
  name: myapp
  version: 1.2.3
  source: https://artifacts.example.com/myapp/myapp-1.2.3.tar.gz
```

## Cosa offre

<div class="grid cards" markdown>

-   :material-database-sync-outline:{ .lg .middle } **Workflow durevoli**

    ---

    Ogni step è registrato da DBOS: se un worker si interrompe, il deploy riprende dall'ultimo step completato e non esegue due volte ciò che è già stato fatto.

    [:octicons-arrow-right-24: Architettura](concetti/architettura.md)

-   :material-pipe:{ .lg .middle } **Pipeline configurabile**

    ---

    Segreti, artefatti, QA, deploy, test post deploy e attivazione. Gli step obbligatori non si possono saltare; quelli opzionali si attivano dal descrittore.

    [:octicons-arrow-right-24: Pipeline e step](concetti/pipeline.md)

-   :material-puzzle-outline:{ .lg .middle } **Plugin di deploy**

    ---

    Il deploy vero e proprio è delegato a plugin: [remote-copy](plugins/remote-copy.md) per file via SSH, [container](plugins/container.md) per Docker e Podman su VM, [kubernetes](plugins/kubernetes.md) per i cluster. Se ne scrivono di nuovi, anche come binari separati via gRPC.

    [:octicons-arrow-right-24: Scrivere un plugin](plugins/index.md)

-   :material-monitor-dashboard:{ .lg .middle } **Interfaccia web**

    ---

    Deploy, ondate e log in tempo reale, approvazioni, inventario con promozioni e rollback, freeze e metriche DORA, con i permessi dell'API. Incorporata nel server, accessibile, in inglese e italiano.

    [:octicons-arrow-right-24: Interfaccia web](guida/interfaccia-web.md)

-   :material-tune-variant:{ .lg .middle } **Configurazione per ambiente**

    ---

    Target, plugin e parametri di ogni ambiente possono stare in Consul. Il descrittore indica solo cosa rilasciare.

    [:octicons-arrow-right-24: Configurazione di ambiente](guida/configurazione-ambiente.md)

-   :material-calendar-clock-outline:{ .lg .middle } **Pianificazione**

    ---

    Deploy a un orario preciso o ricorrenti (cron), sulla stessa coda e con gli stessi limiti di concorrenza dei deploy immediati.

    [:octicons-arrow-right-24: Deploy pianificati](guida/pianificazione.md)

-   :material-flask-outline:{ .lg .middle } **Dry-run e callback**

    ---

    Simula un deploy verificando segreti, artefatto e target senza modificarli. Notifica l'esito di ogni step a endpoint HTTP esterni.

    [:octicons-arrow-right-24: Dry-run](guida/dry-run.md)

-   :material-shield-lock-outline:{ .lg .middle } **Sicurezza e audit**

    ---

    Utenti OIDC e token per la CI, permessi per applicazione e ambiente, audit con catena di hash, segreti da Vault e limiti su ciò che un descrittore può chiedere.

    [:octicons-arrow-right-24: Accesso e audit](operazioni/accesso.md)

-   :material-backup-restore:{ .lg .middle } **Rollback automatico**

    ---

    Se un deploy fallisce, riporta l'ambiente alla release precedente. Le policy stabiliscono per quali tipi di artefatto è possibile.

    [:octicons-arrow-right-24: Rollback](guida/rollback.md)

-   :material-source-branch-check:{ .lg .middle } **Governance dei rilasci**

    ---

    Inventario di cosa è in esercizio, promozione obbligatoria tra ambienti, approvazioni, finestre di freeze e rollback manuale.

    [:octicons-arrow-right-24: Governance](guida/governance.md)

-   :material-earth:{ .lg .middle } **Rilasci a ondate**

    ---

    Canary, poi una region e un'AZ alla volta, con tempo di osservazione e health gate su sonde, Prometheus e Alertmanager. Ogni region eseguita dai suoi worker.

    [:octicons-arrow-right-24: Rilasci a ondate](guida/ondate.md)

-   :material-chart-timeline-variant:{ .lg .middle } **Pronto per l'esercizio**

    ---

    Metriche Prometheus, tracing OpenTelemetry, log di ogni deploy e metriche DORA. Recovery dei worker persi, drain, retention, immagini container, chart Helm e rilasci firmati.

    [:octicons-arrow-right-24: Osservabilità](operazioni/osservabilita.md)

</div>

## Da dove iniziare

<div class="grid cards" markdown>

-   :material-rocket-launch-outline:{ .lg .middle } **[Avvio rapido](avvio-rapido.md)**

    Compila manent ed esegui il primo deploy in locale.

-   :material-console:{ .lg .middle } **[Client `manent`](guida/cli.md)**

    I comandi per sviluppatori e pipeline CI.

-   :material-server-outline:{ .lg .middle } **[`manent-server`](operazioni/server.md)**

    API, worker e operazioni di piattaforma.

-   :material-file-code-outline:{ .lg .middle } **[Descrittore di deploy](guida/descrittore.md)**

    Il riferimento completo del formato.

</div>
