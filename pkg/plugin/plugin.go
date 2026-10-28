// Package plugin defines the public contract every deploy plugin must honor.
// Teams writing new plugins depend only on this package.
package plugin

import (
	"context"
	"errors"
	"log/slog"
)

// Deployer is the interface every deploy plugin must implement.
type Deployer interface {
	// Name returns the unique name of the plugin (e.g. "remote-copy").
	Name() string
	// Supports reports whether the plugin handles the given artifact type.
	Supports(artifactType string) bool
	// Validate checks the request before the deploy, without side effects.
	Validate(req *Request) error
	// Deploy runs the deploy. It must be idempotent: on recovery it can run again.
	Deploy(ctx context.Context, req *Request) (*Result, error)
}

// DryRunner is an optional interface of the Deployer: in dry-run mode the plugin checks that
// the deploy can run (target reachability, credentials, permissions) without changing
// anything. If the plugin does not implement it, in dry-run only Validate is called.
type DryRunner interface {
	DryRun(ctx context.Context, req *Request) (*Result, error)
}

// ErrDryRunUnsupported can be returned by DryRun when support is known only at runtime (e.g.
// an external plugin): manent treats it as a plugin without DryRunner.
var ErrDryRunUnsupported = errors.New("dry-run not supported by the plugin")

// ErrPermanent marks the errors a retry cannot fix (an invalid request, a change refused by a
// safety limit): manent fails the step at once, without retries. Wrap an error with Permanent.
var ErrPermanent = errors.New("permanent plugin error")

// permanentError keeps the message of the original error.
type permanentError struct{ err error }

func (e permanentError) Error() string        { return e.err.Error() }
func (e permanentError) Unwrap() error        { return e.err }
func (e permanentError) Is(target error) bool { return target == ErrPermanent }

// Permanent marks err as permanent (errors.Is(err, ErrPermanent)); nil stays nil. External
// plugins carry the mark over gRPC.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// Factory builds a Deployer from its application configuration.
type Factory func(cfg map[string]any, logger *slog.Logger) (Deployer, error)

// SecretResolver lets the plugin read a secret by name only when needed, so the value is never
// persisted in step outputs.
type SecretResolver interface {
	Secret(ctx context.Context, name string) (string, error)
}

// Artifact describes the artifact already fetched and available locally.
type Artifact struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	LocalPath string `json:"local_path"`
	SHA256    string `json:"sha256"`
}

// Target identifies a deploy destination.
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	// Labels classify the target (region and az are reserved, used by wave rollouts): a plugin
	// can use them, for example to group hosts in an inventory.
	Labels map[string]string `json:"labels,omitempty"`
}

// Request holds everything the plugin needs to run the deploy.
type Request struct {
	DeploymentID string
	// Tenant, Application, Artifact.Name and Environment identify the release: two tenants can
	// have applications with the same name, and every artifact of an application is a release of
	// its own. Tenant is "untenanted" for the applications no tenant lists. Plugins that keep
	// state per release (history, markers) must key it on all four.
	Tenant      string
	Application string
	Environment string
	Artifact    Artifact
	Targets     []Target
	// Params are plugin-specific parameters, taken from the descriptor.
	Params  map[string]string
	Secrets SecretResolver
	// Traffic is set only in traffic percentage rollouts (SlotDeployer).
	Traffic *TrafficRequest
	// Parallelism is the number of targets the plugin may work on at the same time (0 or 1:
	// one after the other). manent sets it above 1 only for plugins that implement
	// ParallelDeployer; ForEachTarget honors it.
	Parallelism int
}

// TargetResult is the deploy outcome on a single target.
type TargetResult struct {
	Host    string            `json:"host"`
	Status  string            `json:"status"`
	Details map[string]string `json:"details,omitempty"`
	// DurationMS is the time spent on the target, set by ForEachTarget.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// Result is the overall outcome returned by the plugin. It must be JSON serializable.
type Result struct {
	Plugin  string         `json:"plugin"`
	Targets []TargetResult `json:"targets"`
	// Outputs are values produced by the deploy (e.g. the hosts of the VMs created) that other
	// deploys can import, if the descriptor or the environment configuration declares them as
	// exports. DBOS persists them: they must never contain secrets. Keys: letters, digits, '_',
	// '-', '.' (max 64 characters, at most MaxOutputs).
	Outputs map[string]string `json:"outputs,omitempty"`
}

// Limits of the deploy outputs (Result.Outputs).
const (
	MaxOutputs     = 64
	MaxOutputBytes = 64 << 10
)

// Possible states of a TargetResult.
const (
	StatusDeployed = "deployed"
	StatusFailed   = "failed"
	// StatusReady means, in dry-run, a target on which the deploy can run.
	StatusReady = "ready"
	// StatusRolledBack means a target brought back to the previous release.
	StatusRolledBack = "rolled_back"
	// StatusSkipped means a target never started because another target of the batch failed.
	StatusSkipped = "skipped"
)
