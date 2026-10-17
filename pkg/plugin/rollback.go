package plugin

import (
	"context"
	"errors"
	"fmt"
)

// Release is a version of the application in service in an environment.
// DBOS persists it: Details must not contain secrets.
type Release struct {
	Version string            `json:"version"`
	Details map[string]string `json:"details,omitempty"`
}

// Rollbacker is an optional interface of the Deployer: it brings the targets back to the
// given release, the one in service before the deploy. It is called when a deploy fails and
// the rollback policy provides for it. It must be idempotent: retries or recoveries can run it
// again, also on targets the deploy had not changed yet.
type Rollbacker interface {
	Rollback(ctx context.Context, req *Request, previous Release) (*Result, error)
}

// RollbackChecker is implemented by Deployers that know rollback support only at runtime
// (e.g. an external plugin, which declares it when the process starts).
type RollbackChecker interface {
	RollbackSupported(ctx context.Context) (bool, error)
}

// ErrRollbackUnsupported means a plugin that does not support rollback.
var ErrRollbackUnsupported = errors.New("rollback not supported by the plugin")

// CanRollback reports whether the Deployer supports rollback.
func CanRollback(ctx context.Context, d Deployer) (bool, error) {
	if c, ok := d.(RollbackChecker); ok {
		return c.RollbackSupported(ctx)
	}
	_, ok := d.(Rollbacker)
	return ok, nil
}

// ReleaseInspector is an optional interface of the Deployer: it reads from the targets the
// release currently in service, without changing them. It returns nil if there is none
// (first deploy). The "plugin" locator uses it.
type ReleaseInspector interface {
	CurrentRelease(ctx context.Context, req *Request) (*Release, error)
}

// ReleaseLocator finds, before the deploy, the release in service to go back to on rollback.
// Implementations are different strategies: reading it from the targets through the plugin
// (PluginLocator) or asking a company system that records the releases in service.
// It returns nil if there is no previous release.
type ReleaseLocator interface {
	Current(ctx context.Context, dep Deployer, req *Request) (*Release, error)
}

// PluginLocatorName is the name of the locator that queries the deploy plugin.
const PluginLocatorName = "plugin"

// PluginLocator reads the release in service from the targets through the plugin's ReleaseInspector.
type PluginLocator struct{}

// Current delegates to the plugin; an error if the plugin cannot read the release in service.
func (PluginLocator) Current(ctx context.Context, dep Deployer, req *Request) (*Release, error) {
	ins, ok := dep.(ReleaseInspector)
	if !ok {
		return nil, fmt.Errorf("plugin %q cannot read the release in service: use another locator", dep.Name())
	}
	return ins.CurrentRelease(ctx, req)
}
