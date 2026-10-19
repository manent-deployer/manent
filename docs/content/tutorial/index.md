# Tutorials

Short, hands-on tutorials that build on each other. Each one starts from where the previous one ended, uses only real commands and configuration keys, and ends with a check you can run yourself.

!!! tip "Just want to see it run?"
    The [Quick start](../avvio-rapido.md) builds manent and runs a deploy in five minutes. The tutorials explain what happens and why.

## Learning path

<div class="grid cards" markdown>

-   :material-numeric-1-circle-outline:{ .lg .middle } **[First deploy to a Linux server](primo-deploy.md)**

    ---

    Release a tarball over SSH with one directory per version, simulate it with a dry-run, watch an automatic rollback and go back with a manual one.

    *30 minutes · Docker, Go*

-   :material-numeric-2-circle-outline:{ .lg .middle } **[Environments and secrets](ambienti.md)**

    ---

    Move targets and parameters out of the descriptor into Consul, read the SSH key from Vault and limit what a descriptor may ask for.

    *30 minutes · continues tutorial 1*

-   :material-numeric-3-circle-outline:{ .lg .middle } **[Promotion, approvals and freezes](promozione.md)**

    ---

    Turn on authentication and roles, require that production only receives what passed through test, approve with a second person and freeze production during an incident.

    *25 minutes · continues tutorial 2*

-   :material-numeric-4-circle-outline:{ .lg .middle } **[Deploy to Kubernetes](kubernetes.md)**

    ---

    Release manifests or a Helm chart to a cluster with a namespaced service account, follow the rollout and roll back from the history kept in the cluster.

    *30 minutes · a Kubernetes cluster*

-   :material-numeric-5-circle-outline:{ .lg .middle } **[Progressive rollout with health gates](rilascio-progressivo.md)**

    ---

    Release a canary first, then one AZ at a time, with bake times, HTTP probes, a Prometheus error-rate gate, an approval after the canary and automatic rollback.

    *30 minutes · Prometheus*

-   :material-numeric-6-circle-outline:{ .lg .middle } **[Deploy from a CI pipeline](ci.md)**

    ---

    Give CI a dedicated token and role, release from GitHub Actions or GitLab CI with an idempotent job, publish the outcome and gate configuration changes.

    *20 minutes · GitHub or GitLab*

-   :material-numeric-7-circle-outline:{ .lg .middle } **[Run manent in production](produzione.md)**

    ---

    Postgres, several workers, OIDC sign-in, TLS, monitoring, SIEM and backups: the checklist to go from the laptop to a platform service.

    *45 minutes · Kubernetes or VMs*

</div>

## Integrations

Once the basics are in place, the [Integrations](../integrazioni/index.md) section has a step-by-step guide for every external tool: Slack, Microsoft Teams, ServiceNow, Jira, GitHub Actions, GitLab CI, Terraform, Vault, Consul, identity providers, Prometheus and the other metrics systems, artifact repositories and SIEMs.

## Conventions

- Commands are run from the tutorial directory, with `manent` and `manent-server` on the `PATH`.
- Command output and logs are shown as manent prints them: logs and messages in Italian, error messages in English.
- Values to replace are written in angle brackets, for example `<your-cluster>`.
