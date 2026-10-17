---
title: manent
hide:
  - navigation
  - toc
---

<div class="mn-hero" markdown>

<div markdown>

<span class="mn-hero__eyebrow">deploy as code · durable DBOS workflows</span>

# manent

<p class="mn-hero__lead">Ship an artifact to production <strong>exactly as it was tested</strong>: a declarative, durable and auditable pipeline of steps, from the CI commit to the last target.</p>

<p class="mn-hero__lead">The name comes from the Latin <em>manere</em>, "to remain, to stand firm": the artifact never changes along the way.</p>

<div class="mn-hero__actions">
<a class="md-button md-button--primary" href="avvio-rapido/">Quick start →</a>
<a class="md-button" href="concetti/architettura/">Architecture</a>
<a class="md-button" href="guida/descrittore/">Descriptor</a>
</div>

</div>

<div class="mn-terminal">
<div class="mn-terminal__bar"><i></i><i></i><i></i><span>~/myapp — manent</span></div>
<pre><code><span class="p">$</span> manent deploy -f release.yml
<span class="dim">deploy queued: 7c0e… (manent logs -f 7c0e…)</span>
<span class="p">$</span> manent logs -f 7c0e…
<span class="dim">14:03:10</span> INFO  <span class="v">[secrets]</span>    secrets verified
<span class="dim">14:03:12</span> INFO  <span class="v">[artifacts]</span>  sha256=2d27fbdf… verified
<span class="dim">14:03:12</span> INFO  <span class="v">[qa]</span>         QA passed
<span class="dim">14:03:15</span> INFO  <span class="v">[deploy]</span>     release 1.4.0 activated on 3 targets
<span class="dim">14:03:15</span> INFO  <span class="v">[activation]</span> live
<span class="ok">✔ SUCCESS</span>  myapp/prod  1.4.0  <span class="dim">kind=promotion</span></code></pre>
</div>

</div>

<div class="mn-stats">
<div class="mn-stat"><b>Durable</b><span>resumes from the last step</span></div>
<div class="mn-stat"><b>Idempotent</b><span>never the same deploy twice</span></div>
<div class="mn-stat"><b>Governed</b><span>promotions · approvals · freezes</span></div>
<div class="mn-stat"><b>Observable</b><span>metrics · traces · logs · DORA</span></div>
</div>

A deploy is described in a declarative file, the **descriptor**. The `manent` client reads it as YAML and sends it to the `manent-server` API, which queues it; a worker runs it as a durable [DBOS](https://github.com/dbos-inc/dbos-transact-golang) workflow.

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

## What it offers

<div class="grid cards" markdown>

-   :material-database-sync-outline:{ .lg .middle } **Durable workflows**

    ---

    Every step is recorded by DBOS: if a worker stops, the deploy resumes from the last completed step and never runs twice what has already been done.

    [:octicons-arrow-right-24: Architecture](concetti/architettura.md)

-   :material-pipe:{ .lg .middle } **Configurable pipeline**

    ---

    Secrets, artifacts, QA, deploy, post-deploy tests and activation. Mandatory steps cannot be skipped; optional ones are enabled from the descriptor.

    [:octicons-arrow-right-24: Pipeline and steps](concetti/pipeline.md)

-   :material-puzzle-outline:{ .lg .middle } **Deploy plugins**

    ---

    The actual deploy is delegated to plugins: [remote-copy](plugins/remote-copy.md) for files over SSH, [container](plugins/container.md) for Docker and Podman on VMs, [kubernetes](plugins/kubernetes.md) for clusters, [ecs](plugins/ecs.md), [lambda](plugins/lambda.md), [cloudrun](plugins/cloudrun.md), [appservice](plugins/appservice.md) and [containerapps](plugins/containerapps.md) for managed cloud services on AWS, Google Cloud and Azure, [jboss](plugins/jboss.md) for JBoss EAP and WildFly, [windows](plugins/windows.md) for IIS, Windows services, scheduled tasks and MSI packages. You can write new ones, also as separate binaries over gRPC.

    [:octicons-arrow-right-24: Writing a plugin](plugins/index.md)

-   :material-monitor-dashboard:{ .lg .middle } **Web interface**

    ---

    Deploys, waves and logs live, approvals, inventory with promotions and rollbacks, freezes and DORA metrics, with the permissions of the API. Built into the server, accessible, in English and Italian.

    [:octicons-arrow-right-24: Web interface](guida/interfaccia-web.md)

-   :material-tune-variant:{ .lg .middle } **Per-environment configuration**

    ---

    Targets, plugins and parameters of each environment can live in Consul. The descriptor only says what to release.

    [:octicons-arrow-right-24: Environment configuration](guida/configurazione-ambiente.md)

-   :material-calendar-clock-outline:{ .lg .middle } **Scheduling**

    ---

    Deploys at a specific time or recurring (cron), on the same queue and with the same concurrency limits as immediate deploys.

    [:octicons-arrow-right-24: Scheduled deploys](guida/pianificazione.md)

-   :material-flask-outline:{ .lg .middle } **Dry-run and callbacks**

    ---

    Simulate a deploy by checking secrets, artifact and targets without changing them. Report the outcome of every step to external HTTP endpoints.

    [:octicons-arrow-right-24: Dry-run](guida/dry-run.md)

-   :material-shield-lock-outline:{ .lg .middle } **Security and audit**

    ---

    OIDC users and tokens for CI, per-application and per-environment permissions, hash-chained audit, secrets from Vault and limits on what a descriptor may ask for.

    [:octicons-arrow-right-24: Access and audit](operazioni/accesso.md)

-   :material-backup-restore:{ .lg .middle } **Automatic rollback**

    ---

    If a deploy fails, the environment goes back to the previous release. Policies decide for which artifact types this is possible.

    [:octicons-arrow-right-24: Rollback](guida/rollback.md)

-   :material-source-branch-check:{ .lg .middle } **Release governance**

    ---

    Inventory of what is live, mandatory promotion between environments, approvals, freeze windows and manual rollback.

    [:octicons-arrow-right-24: Governance](guida/governance.md)

-   :material-forum-outline:{ .lg .middle } **Chat and change management**

    ---

    Deploy and approval notifications in Slack and Teams, approvals with Slack buttons, change tickets in ServiceNow or Jira opened and closed with each deploy.

    [:octicons-arrow-right-24: Integrations](guida/integrazioni.md)

-   :material-earth:{ .lg .middle } **Wave rollouts**

    ---

    Canary, then one region and one AZ at a time, with bake time and a health gate on probes, Prometheus and Alertmanager. Each region is run by its own workers.

    [:octicons-arrow-right-24: Wave rollouts](guida/ondate.md)

-   :material-chart-timeline-variant:{ .lg .middle } **Production ready**

    ---

    Prometheus metrics, OpenTelemetry tracing, per-deploy logs and DORA metrics. Recovery of lost workers, drain, retention, container images, Helm chart and signed releases.

    [:octicons-arrow-right-24: Observability](operazioni/osservabilita.md)

</div>

## Where to start

<div class="grid cards" markdown>

-   :material-rocket-launch-outline:{ .lg .middle } **[Quick start](avvio-rapido.md)**

    Build manent and run the first deploy locally.

-   :material-console:{ .lg .middle } **[`manent` client](guida/cli.md)**

    The commands for developers and CI pipelines.

-   :material-server-outline:{ .lg .middle } **[`manent-server`](operazioni/server.md)**

    API, workers and platform operations.

-   :material-file-code-outline:{ .lg .middle } **[Deploy descriptor](guida/descrittore.md)**

    The complete format reference.

</div>
