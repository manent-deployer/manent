# Integrazioni

manent sta tra la CI, i repository degli artefatti e i target, e parla con gli strumenti che li circondano: dove si discute dei rilasci, dove si approvano i change, dove stanno segreti e configurazione, dove le metriche decidono se una release è sana. Ogni guida qui sotto è una configurazione passo passo per uno strumento: cosa preparare dalla sua parte, cosa scrivere in `manent.yml` e come verificare che funzioni.

!!! info "Una regola per tutte le integrazioni"
    Le credenziali sono sempre **riferimenti a segreti** (`vault:…`, `file:…`, `env:…`), letti al momento dell'uso e mai scritti in log, output o storico dei deploy. Gli invii verso sistemi esterni sono step durevoli: un deploy ripreso dopo un crash non li perde né li duplica, e una notifica fallita non fa mai fallire un deploy.

## Chat

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **Slack** | Avvio ed esito dei deploy in un canale, con un solo messaggio aggiornato; richieste di approvazione con i pulsanti **Approva** e **Respingi**, che rispettano i permessi di manent. | [Slack](slack.md) |
| **Microsoft Teams** | Adaptive Card per deploy e approvazioni, con un pulsante che apre il deploy nell'interfaccia web. | [Microsoft Teams](teams.md) |

## Change management

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **ServiceNow** | Una change request per deploy, aperta all'avvio e chiusa con l'esito; facoltativamente il deploy attende l'approvazione del change. | [ServiceNow](servicenow.md) |
| **Jira** | Lo stesso con una issue Jira (Cloud o Data Center), con transizioni e un commento con l'esito. | [Jira](jira.md) |

## CI/CD e infrastruttura come codice

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **GitHub Actions** | Un'azione composita che rilascia, valida descrittori o configurazioni candidate, con output e riepilogo del job. | [GitHub Actions](github-actions.md) |
| **GitLab CI** | Un template incluso con `inputs` tipizzati e un report dotenv per i job successivi. | [GitLab CI](gitlab-ci.md) |
| **Terraform** | La release in esercizio come risorsa, il drift nel plan e verifiche della configurazione che fanno fallire il plan. | [Terraform](terraform.md) |
| **Qualunque CI** | `manent deploy --outputs/--summary`, codici di uscita e `--id` idempotente. | [Deploy da una pipeline di CI](../tutorial/ci.md) |

## Configurazione, segreti e artefatti

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **HashiCorp Consul** | La configurazione di ambiente nel KV, letta a ogni deploy con la sua revisione. | [HashiCorp Consul](consul.md) |
| **HashiCorp Vault** | Segreti da KV v1 o v2, con autenticazione token, AppRole o Kubernetes. | [HashiCorp Vault](vault.md) |
| **Nexus, Artifactory, registry OCI** | Sorgenti degli artefatti ammesse, con credenziali per prefisso di URL e CA private. | [Repository degli artefatti](repository-artefatti.md) |

## Identità

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **Keycloak, Microsoft Entra ID, Okta** (qualunque provider OIDC) | Accesso degli utenti ad API, CLI e interfaccia web, con i gruppi tradotti in ruoli. | [Identity provider OIDC](oidc.md) |

## Metriche e health gate

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **Prometheus, Alertmanager** | Health gate su query PromQL e allarmi attivi; monitoraggio di manent stesso. | [Prometheus e Alertmanager](prometheus.md) |
| **Datadog, New Relic, Elastic, Amazon CloudWatch, Azure Monitor** | Gli stessi health gate e l'analisi canary sul sistema di metriche che usi già. | [Sistemi di metriche](metriche.md) |
| **OpenTelemetry** | Una traccia per deploy, dalla richiesta della CI a ogni step sul worker. | [Osservabilità](../operazioni/osservabilita.md) |

## Log e audit

| Strumento | Cosa ottieni | Guida |
|---|---|---|
| **Splunk, Elastic, QRadar, qualunque collettore syslog, journald** | Ogni record di log e ogni voce di audit in RFC 5424 con structured data, o come campi del journal. | [SIEM e syslog](siem.md) |

## Load balancer e router del traffico

Si configurano per ambiente, nella configurazione di ambiente (`traffic`), e sono descritti nelle guide delle strategie che li usano:

| Strumento | Serve per | Guida |
|---|---|---|
| HAProxy, F5 BIG-IP, NGINX Plus, NGINX, AWS ELB, IIS ARR, Windows NLB | Svuotare lotti di target durante un rollout a ondate. | [Svuotamento dal load balancer](../guida/svuotamento.md) |
| Gateway API, Istio, AWS ALB, HAProxy, F5, NGINX Plus, alias Lambda, Cloud Run, App Service, Container Apps | Spostare una percentuale di traffico sulla versione candidata. | [Rilasci per percentuale](../guida/traffico.md) |

## Tutto il resto

- **Webhook.** I [callback](../guida/callback.md) inviano un evento HTTP, eventualmente firmato con HMAC, per ogni step e per l'esito di ogni deploy: il modo generico per alimentare una dashboard, un bot o un portale interno.
- **API HTTP nella pipeline.** Uno [step personalizzato](../guida/step-personalizzati.md) di tipo `http` chiama un sistema esterno durante il deploy, per esempio per invalidare una CDN, e può attenderne l'esito.
- **La tua piattaforma.** Un [plugin esterno](../plugins/esterni.md) rilascia su target che manent non supporta, in qualunque linguaggio che parli gRPC.

Le chiavi di configurazione di ogni integrazione sono nei riferimenti: [Chat e change management](../guida/integrazioni.md), [CI e infrastruttura come codice](../guida/ci-iac.md), [Configurazione di manent](../operazioni/configurazione.md).
