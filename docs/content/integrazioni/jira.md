# Jira

A Jira issue for every real deploy of the applications and environments you choose, on Jira Cloud or Jira Data Center: created when the deploy starts, moved through your workflow's transitions, and commented with the outcome at the end. Optionally the deploy **waits** until the issue reaches an approved status, which is how Jira Service Management approvals are honoured.

!!! abstract "What you need"
    A Jira project for changes (for example `CHG` with an issue type `Change`) · a technical account · a secret store reachable by manent

## 1. Prepare Jira

1. Pick or create the project and the issue type changes will use.
2. Look at the workflow of that issue type and note the **names** of:
    - the transition to take when the deploy starts (for example `Implement`);
    - the transitions for success and failure at the end (`Done`, `Failed`);
    - with approvals, the statuses that mean approved and rejected (`Approved`, `Declined`).
3. Create the credentials:
    - **Jira Cloud:** an account (its email is the `username`) and an [API token](https://id.atlassian.com/manage-profile/security/api-tokens);
    - **Jira Data Center:** a personal access token, without `username`.
4. Give the account permission to browse, create, edit, comment and transition issues in the project.

```sh
vault kv put secret/manent/jira token='…'
```

## 2. Configure the provider

```yaml title="manent.yml"
integrations:
  public_url: https://manent.example.com
  change:
    provider: jira
    jira:
      url: https://acme.atlassian.net
      username: manent@acme.com                 # Jira Cloud only
      token_ref: "vault:secret/data/manent/jira#token"
      project: CHG
      issue_type: Change
      fields:
        components: [{name: "{application}"}]
        priority: {name: Medium}
      transitions: {start: Implement, success: Done, failure: Failed}
      approved_statuses: [Approved]
      rejected_statuses: [Declined]
```

- `fields` are JSON values with the placeholders `{application}`, `{environment}`, `{version}`, `{artifact}`, `{deployment}`, `{requested_by}`. manent already sets `summary` and `description`, which a key in `fields` replaces.
- `labels` in `fields` (a list of strings) are **added** to manent's `manent` and `manent-<deploy id>`: that label is how manent finds the issue again. `project` and `issuetype` are refused at startup: use `project` and `issue_type`.
- A transition that is missing from the workflow is skipped.
- `approved_statuses` and `rejected_statuses` are required with `change.approval: true`.

## 3. Turn it on per environment

```yaml title="manent.yml"
governance:
  rules:
    - name: production
      applications: ["*"]
      environments: [prod]
      change: {approval: true, timeout: 72h}
```

Without `approval` the issue is created and closed around the deploy. With `approval: true` it is created when the deploy is requested, and the deploy waits until the issue reaches one of `approved_statuses` (a status in `rejected_statuses` rejects it). In Jira Service Management, let the approval step of the workflow move the issue to the approved status.

## 4. Check it

```sh
manent deploy -f deploy/prod.yml --no-wait
manent status <id> | jq .change
```

The issue carries the label `manent-<deploy id>`: manent searches it by that label (through `/rest/api/2/search/jql`, or `/search` on older servers) after retries or a recovery, so a deploy never creates two issues. At the end manent adds a comment with the outcome, and the rollback outcome if one ran, then applies the `success` or `failure` transition.

## Behaviour and limits

The same as [ServiceNow](servicenow.md#comportamento-e-limiti): no ticket, no deploy; closing errors are only logged; permanent 4xx errors are not retried; dry-runs never create issues.

Reference: [Chat and change management](../guida/integrazioni.md#change-management).
