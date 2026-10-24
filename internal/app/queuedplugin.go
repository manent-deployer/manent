package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// queuedPlugin returns the factory of the stub of a plugin that runs on its own queue and is
// not served by this process: it takes part in plugin selection with its name and artifact
// types, but never deploys. The engine enqueues the plugin's deploys on its queue, where only
// the workers that serve it dequeue them.
func queuedPlugin(name, queue string, types []string) plugin.Factory {
	return func(map[string]any, *slog.Logger) (plugin.Deployer, error) {
		return &queued{name: name, queue: queue, types: types}, nil
	}
}

type queued struct {
	name, queue string
	types       []string
}

func (q *queued) Name() string                      { return q.name }
func (q *queued) Supports(artifactType string) bool { return slices.Contains(q.types, artifactType) }
func (q *queued) Validate(*plugin.Request) error    { return nil }

func (q *queued) Deploy(context.Context, *plugin.Request) (*plugin.Result, error) {
	return nil, q.elsewhere()
}

func (q *queued) DryRun(context.Context, *plugin.Request) (*plugin.Result, error) {
	return nil, q.elsewhere()
}

// elsewhere is the error of a deploy that reached a worker not serving the plugin: it is
// permanent, another attempt on this worker would end the same way.
func (q *queued) elsewhere() error {
	return plugin.Permanent(fmt.Errorf("plugin %s runs only on the workers that serve its queue %s (dbos.plugins), and this worker does not", q.name, q.queue))
}
