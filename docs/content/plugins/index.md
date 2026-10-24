# Writing a plugin

The actual deploy is delegated to plugins. A plugin depends only on the public package `github.com/manent-deployer/manent/pkg/plugin` and can be:

- **internal**: compiled into manent and registered in `internal/app`;
- **external**: a separate binary that manent starts and talks to over gRPC. See [External plugins](esterni.md).

The plugin code is the same in both cases: only the way it is distributed changes.

## The contract

```go
type Deployer interface {
	// Name returns the unique name of the plugin (e.g. "remote-copy").
	Name() string
	// Supports says whether the plugin handles the given artifact type.
	Supports(artifactType string) bool
	// Validate checks the request before the deploy, without side effects.
	Validate(req *plugin.Request) error
	// Deploy runs the deploy. It must be idempotent.
	Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}

// Factory builds a Deployer from its configuration in manent.yml.
type Factory func(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error)
```

### The request

```go
type Request struct {
	DeploymentID string
	Tenant       string            // untenanted for the applications no tenant lists
	Application  string
	Environment  string
	Artifact     Artifact          // Type, Name, Version, LocalPath, SHA256
	Targets      []Target          // Host, Port, User
	Params       map[string]string // deploy.params of the effective descriptor
	Secrets      SecretResolver
}
```

`Tenant`, `Application`, `Artifact.Name` and `Environment` identify the release: a plugin that keeps state per release (history, markers on the targets) must key it on all four, because two [tenants](../guida/tenant.md) can have applications with the same name and every artifact of an application is a release of its own. `Tenant` is `untenanted` for the applications no tenant lists. `Artifact.LocalPath` is the path of the artifact already downloaded and verified on the worker. `Params` are the plugin parameters, after the merge with the [environment configuration](../guida/configurazione-ambiente.md).

### The result

```go
type Result struct {
	Plugin  string
	Targets []TargetResult // Host, Status, Details
	Outputs map[string]string
}
```

`Status` is `plugin.StatusDeployed` or `plugin.StatusFailed` (in a dry-run `plugin.StatusReady`, in a rollback `plugin.StatusRolledBack`). `Details` collects useful information for each target, for example the remote path. The result is serialized as JSON and saved in the output of the `deploy` step.

`Outputs` are values produced by the deploy that other deploys can import, for example the hosts of the VMs a provisioning plugin created (a JSON list) or the address of a database. They are saved in the output of the `deploy` step as `out.<key>`, and become exports only if the descriptor or the environment configuration declares them (`exports: {vms: {from: deploy.vms, format: targets}}`), checked against a format and a pattern: see [Exports and imports](../guida/catene.md#export-import). Keys: letters, digits, `_`, `-`, `.` (at most 64 keys, values up to 64 KiB). Like the rest of the result they are persisted: never put secrets in them. External plugins return them in the `outputs` field of `DeployResponse`.

## Rules

!!! danger "Idempotency"
    `Deploy` can be rerun: by the step's retries (`max_retries`) and by recovery after a worker crash. Running it twice must produce the same final state. For example, write to a temporary file and then rename it, or check the current state before changing it.

- **Secrets:** read them with `req.Secrets.Secret(ctx, name)` only when needed. The name is the logical one from the `secrets` section, typically passed through a parameter (e.g. `ssh_key_secret: deploy-key`). Secrets must never go into the `Result` or the logs.
- **Validate:** checks parameters and targets without contacting the targets. It is called before every `Deploy` and `DryRun`.
- **Context:** `ctx` is cancelled when the deploy is cancelled or the worker shuts down. Long operations must honour it.
- **Logs:** use the logger received by the Factory. Logs reach the application log and the SIEM.
- **Concurrency:** the same instance can serve several deploys at once, on different environments. The plugin must be safe for concurrent use.

## Dry-run { #dry-run }

A plugin can also implement the optional `plugin.DryRunner` interface:

```go
type DryRunner interface {
	DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error)
}
```

In a [dry-run](../guida/dry-run.md), the `deploy` step calls `DryRun` instead of `Deploy`. `DryRun` must check that the deploy can be performed (target reachability, credentials, permissions) **without changing anything**, and report `plugin.StatusReady` for the targets that are ready. It is better to check all targets instead of stopping at the first error, so the outcome is complete.

A plugin that does not implement `DryRunner`, or that returns `plugin.ErrDryRunUnsupported`, is only validated in a dry-run.

## Rollback { #rollback }

To support [rollback](../guida/rollback.md) a plugin implements two optional interfaces:

```go
// Rollbacker brings the targets back to the given release. It must be idempotent.
type Rollbacker interface {
	Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error)
}

// ReleaseInspector reads the live release from the targets, without changing them.
// It returns nil if there is none. Used by the "plugin" locator.
type ReleaseInspector interface {
	CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error)
}
```

- **`Rollback`** receives the same request as the deploy (same targets, parameters and secrets) and the release recorded before the deploy. It must be applied to **all** targets, including those the failed deploy had not touched yet: this is why it must be idempotent. It is better to continue even if a target fails and report the errors at the end, so as many targets as possible are restored.
- **`CurrentRelease`** is called in the checkpoint, before the deploy. If the targets disagree on the live release it is better to return an error than to pick one at random.
- If the plugin implements `Rollbacker` but not `ReleaseInspector`, rollback works with a locator other than `plugin`, for example a corporate release system.

Without `Rollbacker`, a deploy with rollback enabled stops in the checkpoint, before touching the targets.

## Registering an internal plugin

In `internal/app/app.go`, function `NewPlugins`:

```go
if err := reg.Register(myplugin.Name, myplugin.Factory); err != nil {
	return nil, err
}
```

Then enable it in `manent.yml`, in the `plugins` section, with its configuration:

```yaml
plugins:
  my-plugin:
    option: value
```

Only plugins listed in `plugins` are active. Which one to use for a deploy is decided by the policy: `deploy.plugin` if set, otherwise the first active plugin, in alphabetical order, that supports `artifact.type`.

## Testing a plugin

The reference plugin, `plugins/remotecopy`, hides the SSH connection behind the `Dialer` and `Session` interfaces. This way tests replace the network with a fake implementation and check commands and uploads without a real server. It is the recommended pattern for any plugin that talks to external systems.
