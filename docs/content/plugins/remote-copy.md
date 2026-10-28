# remote-copy

The `remote-copy` plugin copies the artifact to remote servers over SSH/SFTP, verifies its integrity and can run a command after the copy. It is compiled into manent and is also available as an external binary (`bin/manent-plugin-remote-copy`, with `make plugins`).

## Configuration

```yaml title="manent.yml"
plugins:
  remote-copy:
    known_hosts: ~/.ssh/known_hosts      # default
    insecure_ignore_host_key: false      # true only in development
    artifact_types: [application]        # default
```

| Key | Description |
|---|---|
| `known_hosts` | File with the accepted host keys. Default `~/.ssh/known_hosts`. |
| `insecure_ignore_host_key` | Disables host key verification. Development only. |
| `artifact_types` | Artifact types handled. Default `[application]`. |

## Deploy parameters

They are set in `deploy.params`, in the descriptor or in the environment configuration.

| Parameter | Required | Description |
|---|---|---|
| `dest_dir` | yes | Remote destination directory. Created if it does not exist. |
| `ssh_key_secret` | one of the two | Name of the secret with the PEM private key. |
| `password_secret` | one of the two | Name of the secret with the password. |
| `post_command` | no | Command run on each target after the copy. |
| `verify_checksum` | no | `false` disables the remote sha256 check. |
| `layout` | no | `flat` (default): the file is copied into `dest_dir`. `releases`: one directory per version and the `current` symlink; required for [rollback](#rollback). |
| `keep_releases` | no | With `layout: releases`, how many releases to keep, including the active one (default 5, minimum 2). |
| `prepare_command` | no | With `layout: releases`, command run in the new release **before** the switch (e.g. extracting the archive). |
| `rollback_command` | no | Command run on each target after the rollback. Default: `post_command`. |

Every target must have a `user`; the default port is 22.

```yaml
secrets:
  - name: deploy-key
    ref: env:DEPLOY_SSH_KEY
deploy:
  plugin: remote-copy
  targets:
    - host: 192.168.1.10
      user: deploy
  params:
    dest_dir: /opt/myapp/releases
    ssh_key_secret: deploy-key
    post_command: "tar -xzf /opt/myapp/releases/myapp-1.2.3.tar.gz -C /opt/myapp/current"
```

## How it works

For each target (one after the other, or [`parallel`](../guida/ondate.md#parallel-targets) at a time):

1. opens the SSH connection, with the key or password read from the secrets at connection time;
2. creates `dest_dir` (`mkdir -p`);
3. uploads the artifact to `<dest_dir>/<file name>.part` and renames it atomically to the final name. An interrupted copy never leaves a partial file under the final name;
4. if the artifact has a sha256 and `verify_checksum` is not `false`, runs `sha256sum` on the remote file and compares it;
5. runs `post_command`, if set.

After the first target that fails no other target starts; the ones already running finish. Rerunning it overwrites the remote file with the same contents, so it is idempotent. `post_command` must be idempotent too, because a retry runs it again.

## `releases` layout

With `layout: releases` each version has its own directory and the `current` symlink points to the live one:

```
/opt/myapp/
├── current -> releases/1.5.0
└── releases/
    ├── 1.3.0/
    ├── 1.4.0/
    │   └── myapp-1.4.0.tar.gz
    └── 1.5.0/
        └── myapp-1.5.0.tar.gz
```

For each target:

1. reads the current release (`readlink current`);
2. uploads the artifact to `releases/<artifact.version>/`, with the same atomic copy and sha256 check as the `flat` layout;
3. runs `prepare_command`, if set;
4. moves `current` to the new release with an atomic symlink swap (`ln -sfn` on a temporary link and `mv -T`, which requires GNU coreutils);
5. runs `post_command`;
6. deletes the oldest releases beyond `keep_releases`, never touching the one just activated and the previous one. A cleanup error is logged but does not fail the deploy.

`artifact.version` is required and becomes the directory name: letters, digits, `.`, `_`, `+` and `-` are allowed.

The commands (`prepare_command`, `post_command`, `rollback_command`) receive these variables:

| Variable | Value |
|---|---|
| `MANENT_RELEASE_DIR` | Release directory, for example `/opt/myapp/releases/1.5.0`. |
| `MANENT_ARTIFACT` | Path of the uploaded artifact. Empty in `rollback_command`. |
| `MANENT_VERSION` | Release version. |
| `MANENT_CURRENT` | Path of the `current` symlink. |

```yaml
deploy:
  plugin: remote-copy
  params:
    dest_dir: /opt/myapp
    ssh_key_secret: deploy-key
    layout: releases
    keep_releases: "5"
    prepare_command: 'tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"'
    post_command: "sudo systemctl restart myapp"
```

!!! warning "Moving from the `flat` layout"
    If `dest_dir/current` already exists as a real directory, the symlink switch fails. Before the first deploy with `layout: releases` it must be renamed or removed.

## Rollback { #rollback }

`remote-copy` supports [rollback](../guida/rollback.md) only with `layout: releases`.

- **Live release** (`plugin` locator): the release `current` points to. Targets without `current`, for example just added, are ignored. If the targets point to different releases the deploy stops in the checkpoint, because it would not be clear which one to go back to.
- **Rollback:** for each target it checks that `releases/<previous version>` exists, moves `current` to it and runs `rollback_command` (or `post_command`). It continues even if a target fails and reports the errors at the end. The target is `rolled_back` or `failed`. A target where the previous release no longer exists fails.

With a corporate release system as locator, the given version must still be present on the targets: size `keep_releases` accordingly.

## Dry-run

In a [dry-run](../guida/dry-run.md), for each target it:

- connects with the same credentials;
- checks, without creating anything, that `dest_dir` exists and is writable, or that it can be created in the first existing directory of the path;
- if checksum verification is on, checks that `sha256sum` is available;
- does **not** copy the artifact and does **not** run `post_command` or `prepare_command`;
- with `layout: releases` reports the live release in `current`.

The dry-run checks all targets and reports the state of each (`ready` or `failed`).

## Result

```json
{
  "plugin": "remote-copy",
  "targets": [
    {
      "host": "192.168.1.10",
      "status": "deployed",
      "details": {"remote_path": "/opt/myapp/releases/myapp-1.2.3.tar.gz", "checksum": "verified", "post_command": "ok"},
      "duration_ms": 4210
    }
  ]
}
```

A target's `status` is `deployed`, `failed`, or `skipped` when it never started because another target of the batch failed; `duration_ms` is the time spent on it.
