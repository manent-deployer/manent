# Slack

Deploy and approval notifications in Slack channels. With a bot token the start message is **updated in place** with the outcome, so a channel shows one message per deploy; approval requests carry **Approve** and **Reject** buttons that go through the same permission checks as the API.

!!! abstract "What you need"
    Admin rights to create an app in your Slack workspace · a secret store reachable by manent (Vault in the examples) · for approval buttons, the manent address reachable from Slack over HTTPS

## 1. Create the Slack app

1. At [api.slack.com/apps](https://api.slack.com/apps) choose **Create New App → From scratch**, name it `manent` and pick the workspace.
2. In **OAuth & Permissions → Bot Token Scopes** add `chat:write`. Add `users:read.email` too if you will map Slack users to manent identities by email (step 5).
3. **Install to Workspace** and copy the **Bot User OAuth Token** (`xoxb-…`).
4. In **Basic Information → App Credentials** copy the **Signing Secret**: manent uses it to verify that button clicks really come from Slack.
5. Invite the bot to every channel it will write to: `/invite @manent` in the channel.

Store both values in the secret store:

```sh
vault kv put secret/manent/slack bot_token=xoxb-… signing_secret=…
```

## 2. Configure notifications

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com      # messages link to the deploy page
  language: en                                # en (default) or it
  slack:
    bot_token_ref: "vault:secret/data/manent/slack#bot_token"
    signing_secret_ref: "vault:secret/data/manent/slack#signing_secret"
  notifications:
    - name: prod
      applications: ["*"]
      environments: [prod]
      slack: {channel: C0123456789}           # channel ID: channel details → About
    - name: payments-team
      applications: ["pay-*"]
      environments: ["*"]
      events: [deployment]                    # only start and outcome, no approvals
      slack: {channel: C0456789012}
```

- Every entry is a **destination** for a set of applications and environments; a deploy goes to every destination that matches it.
- `events` is `deployment` (start and outcome) and/or `approval` (request and decision). Default: both.
- Dry-runs are not notified unless the destination has `dry_run: true`.

Restart manent. Destinations are fixed when a deploy is enqueued: deploys already in the queue and existing recurring schedules keep the destinations they were created with.

!!! tip "No bot? Use an incoming webhook"
    `slack: {webhook_ref: "vault:…#webhook_url"}` instead of `channel` posts one message per event to an incoming webhook. Messages are not updated, and approval buttons are not available.

## 3. Check it

Add a temporary destination that also notifies dry-runs, and run one:

```yaml
    - name: slack-test
      applications: [hello]
      environments: [test]
      dry_run: true
      slack: {channel: C0123456789}
```

```sh
manent deploy -f deploy/test.yml --dry-run
manent steps <id> | jq -r '.[] | select(.name | startswith("notify:")) | "\(.name) \(.error // "ok")"'
```

Each delivery is a DBOS step named `notify:<destination>:<event>`. A failed delivery shows its error there and in the worker log (`chat notification not sent`, also sent to the SIEM), but never fails the deploy.

## 4. Approvals from Slack

With `signing_secret_ref` configured, manent registers `POST /integrations/slack/interactions`. In the Slack app:

1. **Interactivity & Shortcuts → On**.
2. **Request URL:** `https://manent.example.com/integrations/slack/interactions`.

When a deploy needs [approvals](../guida/governance.md#approvazioni), destinations with the `approval` event receive a message with **Approve** and **Reject**. A click:

1. is verified with the signing secret and refused if older than 5 minutes;
2. is attributed to a **manent identity** (next step);
3. is recorded as that identity's decision, with the same rules as `manent approve`: the `deployment.approve` permission and no approval of one's own deploy;
4. gets an answer visible only to the user who clicked: decision recorded, or why it was refused.

When the approval ends (approved, rejected or expired) the message is updated: buttons removed, decisions listed.

!!! note "Reachability"
    Slack calls the Request URL from the Internet. If manent is internal, publish only this path through your reverse proxy or API gateway; everything else can stay private.

## 5. Map Slack users to identities

A Slack user not linked to an identity cannot decide anything. Two ways:

=== "Explicit list"

    ```yaml
    integrations:
      slack:
        identities:
          U024BE7LH: {subject: "oidc:alice", groups: [cab]}
          U031XQ7PZ: {subject: "oidc:bob", groups: [cab]}
    ```

    The Slack member ID is in the user's profile (**⋮ → Copy member ID**). `groups` are those used by RBAC bindings.

=== "By email"

    ```yaml
    integrations:
      slack:
        email_identity: "oidc:{email}"     # requires the users:read.email scope
    ```

    The email in the Slack profile becomes `oidc:alice@example.com`. Use it only if Slack emails are those of your identity provider, and if your `username_claim` produces emails.

!!! warning "The mapping is part of your security"
    Whoever is linked to `oidc:alice` approves as Alice. Keep `identities` in the configuration managed by the platform team, and review it like an access list.

## Troubleshooting

| Symptom | Check |
|---|---|
| No message at all | The destination's `applications`/`environments` match the deploy; the bot is a member of the channel; the `notify:` step error in `manent steps`. |
| A new message per event instead of an update | The destination uses `webhook_ref`, or the bot token is missing. |
| Buttons do nothing | Interactivity is on, the Request URL is reachable from Slack, `signing_secret_ref` is set (otherwise the endpoint is not registered). |
| "Not linked" or permission refused | The user is in `identities` (or `email_identity` resolves), and that identity has `deployment.approve` on the application and environment. |

Reference: [Chat and change management](../guida/integrazioni.md#notifiche).
