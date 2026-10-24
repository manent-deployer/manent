# ansible

The `ansible` plugin releases system configuration: the artifact is an Ansible project (playbooks, roles and collections), manent builds the inventory from the targets each run receives, and a deploy runs `ansible-playbook`. Configuration changes then go through the same pipeline as applications: waves and batches, approvals, freezes, audit, inventory and [deploy chains](../guida/catene.md). A [dry-run](../guida/dry-run.md) is check mode.

It is an [external plugin](esterni.md), developed in its own repository: [manent-deployer/manent-plugin-ansible](https://github.com/manent-deployer/manent-plugin-ansible).

## Where it runs

An external plugin is a child process of the worker, so Python and `ansible-core` must be on the worker's machine. manent's distroless image has neither: run the plugin on a pool of workers built from the `server-ansible` target of the Dockerfile (Python, `ansible-core`, the `ansible.posix` and `community.general` collections, the plugin binary; `ANSIBLE_COLLECTIONS` adds more), and give the plugin its [own queue](../operazioni/scalabilita.md#plugin-queues):

```yaml title="manent.yml (every process)"
plugins:
  ansible:
    external:
      command: /usr/local/bin/manent-plugin-ansible
      sha256: "<sha256 of the binary>"
    artifact_types: [ansible]
    queue: ansible
```

```yaml title="manent.yml (only the workers of the pool)"
dbos:
  plugins: [ansible]
```

```sh
docker build --target server-ansible -t manent-server-ansible .
```

The API and the distroless workers know the plugin by name and artifact types and never start it.

## How a deploy runs

1. The project is extracted into a directory of the run; the playbook (`site.yml` by default) may sit at the root of the archive or in its only top-level directory. Collections in `collections/` and roles in `roles/` are found automatically.
2. The inventory lists the targets of the run with their connection variables, and one group per label: `region=eu-1` becomes the group `region_eu_1`, `az=eu-1a` the group `az_eu_1a`. manent's [waves](../guida/ondate.md) and `maxUnavailable` replace `serial`: each run receives only the targets of its batch.
3. `ansible-playbook` runs with argv arguments (no shell), the JSON callback and an empty configuration file: neither `/etc/ansible/ansible.cfg` nor an `ansible.cfg` in the artifact is read.
4. A failed or unreachable host fails the run, with the totals of every host in the error and in the result (`ok`, `changed`, `failed`, `unreachable`, `skipped`). A syntax error, a missing role or invalid options fail it without retries.
5. With `release_marker: true`, a small playbook records the version on the targets.

Playbooks must be idempotent: retries and recoveries can run them again.

## The artifact

```sh
ansible-galaxy collection install -r requirements.yml -p collections
tar -czf os-config-2.1.0.tgz site.yml roles collections group_vars
```

```yaml title="deploy.yml"
apiVersion: manent/v1
application: os
environment: prod
artifact:
  type: ansible
  name: os-config
  version: 2.1.0
  source: https://nexus.example.com/repository/ansible/os-config-2.1.0.tgz
```

## Configuration

```yaml title="Environment configuration: os/os-config/prod"
plugin: ansible
targets:
  - {host: web-1.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1a}}
  - {host: web-2.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1b}}
params:
  playbook: site.yml
  ssh_key_secret: deploy-key
  become: "true"
  var.ntp_server: ntp.example.com
  secret_var.db_password: db-password
  vault_password_secret: vault-pass
  release_marker: "true"
secrets:
  - {name: deploy-key, ref: "vault:secret/data/os/prod#ssh_key"}
  - {name: db-password, ref: "vault:secret/data/os/prod#db_password"}
  - {name: vault-pass, ref: "vault:secret/data/os/prod#vault_password"}
```

| Parameter | Description |
|---|---|
| `playbook` | Playbook to run, relative to the project (default `site.yml`). |
| `ssh_key_secret` | Secret with the private SSH key, written to a 0600 file of the run. |
| `become` | `true` adds `--become`. |
| `var.<name>` | Extra variable. `manent_*` and `ansible_*` are reserved. |
| `secret_var.<name>` | Extra variable with the value of the named secret. |
| `vault_password_secret` | Secret with the Ansible Vault password (`--vault-password-file`). |
| `forks` | `--forks` (1-500). |
| `tags`, `skip_tags` | Comma-separated `--tags` and `--skip-tags`. |
| `timeout` | Maximum duration of a run (default `1h`). |
| `release_marker` | `true` records the version on the targets after the playbook. |
| `release_marker_dir` | Directory of the markers (default `/var/lib/manent/releases`); written with `become` when it is set. |

Every run also receives `manent_deployment`, `manent_tenant`, `manent_application`, `manent_environment`, `manent_artifact`, `manent_version` and `manent_sha256`. Unknown parameters are refused.

Plugin configuration: `ansible_playbook` (the command, default from `PATH`), `known_hosts` (default `~/.ssh/known_hosts` of the worker), `insecure_ignore_host_key` (development only), `collections_path` (collections of the worker, after those of the artifact; default `/usr/share/ansible/collections`).

## Isolation

- Every run has its own working directory and `HOME`, removed at the end; the process gets only `PATH`, `TZ` and `LANG` from the worker.
- Variables, secret variables, the SSH key and the Vault password are written to 0600 files of the run. Secrets are redacted from error messages and never appear in step outputs; `--diff` is never used, because it could show them.

## Outputs and chains

Data of `ansible.builtin.set_stats` that are not per host become plugin outputs: strings as they are, lists and objects as compact JSON. `set_stats` aggregates the values of every host by default, so use `run_once: true`:

```yaml
- name: export the application directory
  run_once: true
  ansible.builtin.set_stats:
    data: {app_dir: /srv/shop}
```

A typical chain is infrastructure → configuration → application in one [composite release](../guida/catene.md#release-composte): the [terraform](terraform.md) plugin exports the VMs as targets (`format: targets`, with their labels), the ansible deploy reads them with `targetsFrom` and exports what the application needs, which the application deploy reads with `paramsFrom`.

## Release in service, rollback and limits

- With `release_marker: true` the plugin reads the version in service from the markers of the targets (rollback locator, [reconciliation](../guida/governance.md#stato-reale)), and reports an error if the targets disagree. The reconciliation reads it only on a worker that serves the plugin.
- Rollback is manual: `manent rollback` runs the playbook of the previously released version. An automatic rollback with a `rollback_playbook` and `galaxy: install` at deploy time are planned.
