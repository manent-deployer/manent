package steps

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// reconcileID is the deployment ID of the requests built to read the targets: it is not a
// deploy, but plugins use the ID in names (e.g. AWS sessions) and need a valid one.
const reconcileID = "reconcile"

// InspectTargets reads from every target, one at a time and at most concurrency together, the
// version in service through the ReleaseInspector of the plugin chosen by the descriptor's
// policy. versions[i] is "" when target i has no release; errs[i] is the error reading it.
// The error is returned when the environment cannot be read at all (no plugin, or a plugin
// that cannot read the release in service).
func (s Deploy) InspectTargets(ctx context.Context, d *descriptor.Descriptor, targets []descriptor.Target, timeout time.Duration,
	concurrency int) (versions []string, errs []error, err error) {
	dep, _, err := s.selectPlugin(pipeline.Input{Descriptor: d, DeploymentID: reconcileID})
	if err != nil {
		return nil, nil, err
	}
	ins, ok := dep.(plugin.ReleaseInspector)
	if !ok {
		return nil, nil, fmt.Errorf("plugin %q cannot read the release in service", dep.Name())
	}
	versions, errs = make([]string, len(targets)), make([]error, len(targets))
	sem := make(chan struct{}, max(1, concurrency))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			c, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			// The artifact is not downloaded: plugins read the release from the targets only.
			req := &plugin.Request{
				DeploymentID: reconcileID,
				Application:  d.Application,
				Environment:  d.Environment,
				Artifact:     plugin.Artifact{Type: d.Artifact.Type, Name: d.Artifact.Name, Version: d.Artifact.Version},
				Params:       d.Deploy.Params,
				Secrets:      secrets.DescriptorResolver{Provider: s.Secrets, Descriptor: d},
				Targets:      []plugin.Target{{Host: t.Host, Port: t.Port, User: t.User}},
			}
			rel, err := ins.CurrentRelease(c, req)
			switch {
			case err != nil:
				errs[i] = err
			case rel != nil:
				versions[i] = rel.Version
			}
		}()
	}
	wg.Wait()
	return versions, errs, nil
}
