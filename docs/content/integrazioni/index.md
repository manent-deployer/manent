# Integrations

manent sits between your CI, your artifact repositories and your targets, and talks to the tools around them: where people discuss releases, where changes are approved, where secrets and configuration live, where metrics decide whether a release is healthy. Each guide below is a step-by-step setup for one tool: what to prepare on its side, what to write in `manent.yml`, and how to check that it works.

!!! info "One rule for every integration"
    Credentials are always **secret references** (`vault:…`, `file:…`, `env:…`), read when they are used and never written to logs, outputs or the deploy history. Deliveries to external systems are durable steps: a deploy resumed after a crash neither loses nor duplicates them, and a failed notification never fails a deploy.

## Chat

| Tool | What you get | Guide |
|---|---|---|
| **Slack** | Deploy started and outcome in a channel, one message updated in place; approval requests with **Approve** and **Reject** buttons that respect manent permissions. | [Slack](slack.md) |
| **Microsoft Teams** | Adaptive Cards for deploys and approvals, with a button that opens the deploy in the web interface. | [Microsoft Teams](teams.md) |

## Change management

| Tool | What you get | Guide |
|---|---|---|
| **ServiceNow** | A change request per deploy, opened when it starts and closed with its outcome; optionally the deploy waits for the change to be approved. | [ServiceNow](servicenow.md) |
| **Jira** | The same with a Jira issue (Cloud or Data Center), with transitions and an outcome comment. | [Jira](jira.md) |

## CI/CD and infrastructure as code

| Tool | What you get | Guide |
|---|---|---|
| **GitHub Actions** | A composite action that deploys, validates descriptors or candidate configurations, with outputs and job summary. | [GitHub Actions](github-actions.md) |
| **GitLab CI** | A template included with typed `inputs` and a dotenv report for the following jobs. | [GitLab CI](gitlab-ci.md) |
| **Terraform** | The release in service as a resource, drift in the plan, and configuration checks that fail the plan. | [Terraform](terraform.md) |
| **Any CI** | `manent deploy --outputs/--summary`, exit codes and idempotent `--id`. | [Deploy from a CI pipeline](../tutorial/ci.md) |

## Configuration, secrets and artifacts

| Tool | What you get | Guide |
|---|---|---|
| **HashiCorp Consul** | The environment configuration in the KV store, read at every deploy with its revision. | [HashiCorp Consul](consul.md) |
| **HashiCorp Vault** | Secrets from KV v1 or v2, with token, AppRole or Kubernetes authentication. | [HashiCorp Vault](vault.md) |
| **Nexus, Artifactory, OCI registries** | Allowed artifact sources with credentials per URL prefix and private CAs. | [Artifact repositories](repository-artefatti.md) |

## Identity

| Tool | What you get | Guide |
|---|---|---|
| **Keycloak, Microsoft Entra ID, Okta** (any OIDC provider) | User sign-in for the API, the CLI and the web interface, with groups mapped to roles. | [OIDC identity providers](oidc.md) |

## Metrics and health gates

| Tool | What you get | Guide |
|---|---|---|
| **Prometheus, Alertmanager** | Health gates on PromQL queries and active alerts; monitoring of manent itself. | [Prometheus and Alertmanager](prometheus.md) |
| **Datadog, New Relic, Elastic, Amazon CloudWatch, Azure Monitor** | The same health gates and canary analysis on the metrics system you already use. | [Metrics systems](metriche.md) |
| **OpenTelemetry** | A trace per deploy, from the CI request to every step on the worker. | [Observability](../operazioni/osservabilita.md) |

## Logs and audit

| Tool | What you get | Guide |
|---|---|---|
| **Splunk, Elastic, QRadar, any syslog collector, journald** | Every log record and audit entry as RFC 5424 with structured data, or as journal fields. | [SIEM and syslog](siem.md) |

## Load balancers and traffic routers

These are configured per environment, in the environment configuration (`traffic`), and described in the guides of the strategies that use them:

| Tool | Used for | Guide |
|---|---|---|
| HAProxy, F5 BIG-IP, NGINX Plus, NGINX, AWS ELB, IIS ARR, Windows NLB | Draining batches of targets during a wave rollout. | [Load balancer draining](../guida/svuotamento.md) |
| Gateway API, Istio, AWS ALB, HAProxy, F5, NGINX Plus, Lambda aliases, Cloud Run, App Service, Container Apps | Moving a percentage of traffic to the candidate version. | [Percentage rollouts](../guida/traffico.md) |

## Anything else

- **Webhooks.** [Callbacks](../guida/callback.md) send an HTTP event, optionally signed with HMAC, for every step and for the outcome of every deploy: the generic way to feed a dashboard, a bot or an internal portal.
- **HTTP APIs in the pipeline.** A [custom step](../guida/step-personalizzati.md) of type `http` calls an external system during the deploy, for example to purge a CDN, and can wait for its outcome.
- **Your own platform.** An [external plugin](../plugins/esterni.md) releases to targets manent does not support, in any language that speaks gRPC.

The configuration keys of every integration are listed in the reference: [Chat and change management](../guida/integrazioni.md), [CI and infrastructure as code](../guida/ci-iac.md), [Configuring manent](../operazioni/configurazione.md).
