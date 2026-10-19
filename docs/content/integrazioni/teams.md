# Microsoft Teams

Deploy and approval notifications in Teams channels, as Adaptive Cards. Teams webhooks cannot receive clicks, so approval cards carry a **Review in manent** button that opens the deploy in the [web interface](../guida/interfaccia-web.md), where the user signs in and decides with the usual rules.

!!! abstract "What you need"
    A Teams channel where you can add a workflow · a secret store reachable by manent · `integrations.public_url` set, so cards can link to the web interface

## 1. Create the webhook

With the **Workflows** app (the replacement for Office 365 connectors):

1. In the channel, **⋯ → Workflows**, and pick the template **Post to a channel when a webhook request is received**.
2. Choose the team and channel, and create the workflow.
3. Copy the URL Teams shows at the end. Anyone with this URL can post to the channel: treat it as a secret.

A classic channel *Incoming Webhook*, where still available, works the same way.

```sh
vault kv put secret/manent/teams cab_webhook='https://…'
```

## 2. Configure the destination

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com
  language: en
  notifications:
    - name: cab
      applications: ["*"]
      environments: ["prod*"]
      events: [approval]                       # the change advisory board sees only approvals
      teams: {webhook_ref: "vault:secret/data/manent/teams#cab_webhook"}
    - name: ops
      applications: ["*"]
      environments: [prod]
      events: [deployment]
      teams: {webhook_ref: "vault:secret/data/manent/teams#ops_webhook"}
```

The webhook URL is read from the secret store at every delivery, so you can rotate it without restarting manent. Destinations are fixed when a deploy is enqueued.

## 3. What the cards show

| Event | Card |
|---|---|
| `deployment` started | Application, environment, version, requester, change ticket if any, link to the deploy. |
| `deployment` finished | The outcome; for failures the error and the outcome of the automatic rollback. |
| `approval` requested | Who asked, what, how many approvals are needed and until when, with **Review in manent**. |
| `approval` finished | Approved, rejected or expired, with the decisions. |

Each event is a new card: unlike Slack with a bot token, a webhook cannot update a message it posted.

## 4. Check it

Add `dry_run: true` to a destination for a test environment and run `manent deploy --dry-run`. The delivery is the `notify:<destination>:<event>` step in `manent steps <id>`; a failure is recorded there and in the worker log, and never fails the deploy.

!!! note "Interactive buttons in Teams"
    Approving directly inside Teams would need a bot registered in Azure. manent keeps the decision in the web interface, which already enforces sign-in, permissions and the ban on self-approval.

Reference: [Chat and change management](../guida/integrazioni.md#notifiche).
