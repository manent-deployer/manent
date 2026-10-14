# Chat and change management

manent integrates with the tools where releases are discussed and approved:

- **notifications** in Slack and Microsoft Teams: deploy started and outcome, approval requests and decisions;
- **approvals from Slack**, with Approve and Reject buttons that respect the same permissions as the API;
- **change tickets** in ServiceNow or Jira: opened when the deploy starts and closed with its outcome, optionally waiting for the change to be approved before running.

Everything is configured in `manent.yml` under `integrations`; change management is enabled per environment in the [governance rules](governance.md). Like [callbacks](callback.md), every delivery is a DBOS step: a deploy resumed after a crash does not lose or duplicate messages, and a failed notification never fails the deploy.

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com   # links to the web interface in messages and tickets
  language: en                             # en (default) or it
  slack:
    bot_token_ref: "vault:secret/data/manent/slack#bot_token"
    signing_secret_ref: "vault:secret/data/manent/slack#signing_secret"
    identities:
      U024BE7LH: {subject: "oidc:alice", groups: [cab]}
    # email_identity: "oidc:{email}"
  notifications:
    - name: prod
      applications: ["*"]
      environments: [prod]
      slack: {channel: C0123456789}
    - name: cab
      applications: ["*"]
      environments: ["prod*"]
      events: [approval]
      teams: {webhook_ref: "vault:secret/data/manent/teams#cab_webhook"}
  change:
    provider: servicenow
    servicenow:
      url: https://acme.service-now.com
      username: manent
      password_ref: "vault:secret/data/manent/servicenow#password"
      fields: {assignment_group: "Release Management", cmdb_ci: "{application}"}
governance:
  rules:
    - name: prod
      applications: ["*"]
      environments: [prod]
      approvals: {required: 1}
      change: {approval: true, timeout: 72h}
```

## Notifications { #notifiche }

Each entry in `integrations.notifications` is a destination for a set of applications and environments (patterns with `*`). A deploy is sent to **all** the destinations that match it.

| Key | Description |
|---|---|
| `name` | Lower case, digits and `-`. It appears in the DBOS step names (`notify:<name>:<event>`). |
| `applications`, `environments` | Required, `"*"` for all. |
| `events` | `deployment` (start and outcome) and/or `approval` (request and outcome). Default both. |
| `dry_run` | `true` to also notify dry-runs (default no). |
| `slack.channel` | Channel ID or name, with the bot token: the start message is **updated** with the outcome, and approval requests have buttons. |
| `slack.webhook_ref` | Instead of the channel: secret with the URL of an incoming webhook. One message per event, no buttons. |
| `teams.webhook_ref` | Secret with the URL of a Teams webhook: a flow of the Workflows app ("Post to a channel when a webhook request is received") or a channel webhook. Messages are Adaptive Cards. |

Messages report application, environment, version, who requested the deploy, the change ticket and, for failures, the error and the outcome of the automatic rollback. With `public_url` they link to the deploy page in the [web interface](interfaccia-web.md).

The destinations of a deploy are chosen when it is enqueued and saved with the request, like the [plan](../concetti/pipeline.md): changing `integrations.notifications` does not alter deploys already enqueued or [recurring schedules](pianificazione.md) already created. A destination removed from the configuration simply stops receiving messages.

### Slack app

1. Create an app at [api.slack.com/apps](https://api.slack.com/apps) with the bot scope `chat:write` (and `users:read.email` if you use `email_identity`), install it in the workspace and invite the bot to the channels.
2. Save the **Bot User OAuth Token** (`xoxb-…`) and the **Signing Secret** in the secret store and reference them in `bot_token_ref` and `signing_secret_ref`.
3. For approvals, enable **Interactivity** and set the Request URL to `https://<manent>/integrations/slack/interactions`. The address must be reachable from Slack.

## Approvals from chat { #approvazioni-da-chat }

When a deploy needs [approvals](governance.md#approvazioni), the request reaches the destinations with `approval` events.

**Slack.** The message has **Approve** and **Reject** buttons. A click reaches `/integrations/slack/interactions`, which:

1. verifies Slack's signature (`X-Slack-Signature`, HMAC with the signing secret) and rejects requests older than 5 minutes;
2. finds the user's **manent identity**: first in `slack.identities` (Slack user ID → `subject` and `groups`), otherwise, with `email_identity`, from the email in the Slack profile (`oidc:{email}` becomes `oidc:alice@example.com`);
3. records the decision **as that identity**, through the same permission checks as the API: the `deployment.approve` permission on the application and environment, and the ban on approving your own deploy;
4. replies to the user with a message only they can see: decision recorded, or why it was refused.

The channel message is updated when the approval ends (approved, rejected or expired): the buttons disappear and the decisions are listed. A user not linked to an identity cannot decide anything.

!!! warning "The mapping is part of the security"
    Whoever is linked to `oidc:alice` in `slack.identities` approves as Alice. Keep the list in the configuration managed by the operators, and prefer `email_identity` only if the emails in Slack are those of your identity provider. Without the signing secret the endpoint is not registered.

**Teams.** Teams webhooks cannot receive clicks: the card has a **Review in manent** button that opens the deploy in the web interface, where the user logs in and decides with the same rules. Interactive buttons in Teams would need a bot registered in Azure.

## Change management { #change-management }

With `change` in a governance rule, every real deploy of those applications and environments (not dry-runs) has a change ticket:

1. **Opening.** When the deploy starts, the `change:open` step looks for the deploy's ticket and creates it if it does not exist, then moves it to implementation. The ticket is found by the deploy ID (`correlation_id` in ServiceNow, the `manent-<id>` label in Jira), so retries and recoveries never create a second one. **Without a ticket the deploy does not start.**
2. **Closing.** At the end the `change:close` step records the outcome (and the rollback, if any) and closes the ticket. An error at this point is logged: the deploy has already finished.

The ticket number and link appear in the deploy status (`change`), in the CLI, in the web interface and in the chat messages.

### Waiting for the change approval

With `change: {approval: true}` the deploy also waits for the change to be **approved in the ITSM system**:

- the ticket is opened when the deploy is requested, in the [approval workflow](governance.md#approvazioni), which takes no worker slots;
- manent reads its state at growing intervals, from `integrations.change.poll_interval` (default 1 minute) up to 5 minutes;
- the deploy starts when **both** conditions are met: the manent approvals required by `approvals` (if any) and the approved change. A change rejected or cancelled in the ITSM rejects the deploy, with a decision named after the ticket (`servicenow:CHG0031337`);
- the wait expires after `change.timeout` (default 72 hours) or `approvals.timeout`, whichever is longer.

A change already approved when it is created (a pre-approved standard change, for example) does not make the deploy wait.

| `governance.rules[].change` | Default | Description |
|---|---|---|
| `approval` | `false` | Wait for the change approval in the ITSM system. |
| `timeout` | `72h` | Maximum wait. |

### ServiceNow

The Table API on `change_request`, with basic authentication (`username`, `password_ref`) or an OAuth token (`token_ref`).

| `integrations.change.servicenow` | Default | Description |
|---|---|---|
| `url` | | Instance address. |
| `table` | `change_request` | Table of the changes. |
| `fields` | | Extra fields of the ticket (`type`, `assignment_group`, `cmdb_ci`, `category`…). Values accept `{application}`, `{environment}`, `{version}`, `{artifact}`, `{deployment}`, `{requested_by}`. |
| `start_state` | `-1` (Implement) | State set when the deploy starts; `""` to leave it unchanged. |
| `close_states` | `["0", "3"]` (Review, Closed) | States set in order at the end. |
| `close_codes` | `success: successful`, `failure: unsuccessful` | `close_code` for each outcome; `close_notes` describes it. |

The approval is read from the `approval` field (`approved`, `rejected`); a cancelled change (state `4`) counts as rejected. If your state model does not allow the transitions, adapt `start_state` and `close_states`: a refused start is logged and does not block the deploy.

The manent user needs permission to read, create and update records of the table (for example the `itil` role, or a dedicated role with ACLs on `change_request`).

### Jira

The REST API v2, on Jira Cloud (`username` with the email and an API token in `token_ref`) or Jira Data Center (only `token_ref`, a personal access token).

| `integrations.change.jira` | Description |
|---|---|
| `url`, `project`, `issue_type` | Where to create the issue (for example project `CHG`, type `Change`). |
| `fields` | Extra fields, with JSON values and the same placeholders as ServiceNow (for example `components: [{name: "{application}"}]`). |
| `approved_statuses`, `rejected_statuses` | Statuses that approve or reject the change. Required with `approval: true`. |
| `transitions` | Transition names: `start` (deploy started), `success` and `failure` (end). Missing transitions are skipped. |

At the end manent adds a comment with the outcome and applies the transition. Jira Service Management approvals are read through the issue status: the approval step of the workflow moves the issue to one of the `approved_statuses`.

## Reliability

- Notifications and change operations are DBOS steps with retries (`callbacks.max_retries` for notifications, 3 for changes). 4xx errors from the ITSM (except 408 and 429) are permanent and not retried.
- Destinations and change rules are fixed in the request: a configuration change during a recovery does not change the sequence of steps of running workflows.
- Webhook URLs and tokens are read from the secret store at every use and never appear in logs, outputs or messages.
- Redirects are not followed, so credentials never reach another host.
