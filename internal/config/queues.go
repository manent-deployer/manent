package config

import (
	"errors"
	"fmt"
	"slices"

	"github.com/manent-deployer/manent/internal/tenant"
)

// QueueRoute is an entry of queues: the deploys of the matching releases run on the queue Name,
// only on the workers that list it in dbos.queues (for example a pool with GPUs, with access
// to a network segment or with a team's credentials). The first matching entry wins.
type QueueRoute struct {
	Name string `yaml:"name"`
	// Match selects the releases by tenant/application, artifact and environment; at least
	// one pattern is required.
	Match EnvMatch `yaml:"match"`
	// Concurrency is the global limit of the queue (0: dbos.global_concurrency).
	Concurrency int `yaml:"concurrency"`
}

// QueueRoute returns the entry of queues with the name, or nil.
func (c *Config) QueueRoute(name string) *QueueRoute {
	i := slices.IndexFunc(c.Queues, func(r QueueRoute) bool { return r.Name == name })
	if i < 0 {
		return nil
	}
	return &c.Queues[i]
}

// validateQueueRoutes checks queues, dbos.queues and dbos.exclusive.
func (c *Config) validateQueueRoutes() []error {
	var errs []error
	plugins := map[string]bool{}
	for name := range c.Plugins {
		if q := c.PluginQueue(name); q != "" {
			plugins[q] = true
		}
	}
	seen := map[string]bool{}
	for i, r := range c.Queues {
		at := fmt.Sprintf("queues[%d]", i)
		switch {
		case !queueRe.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s.name: invalid queue name %q", at, r.Name))
		case r.Name == c.DBOS.Queue || r.Name == c.DBOS.Queue+"-approvals":
			errs = append(errs, fmt.Errorf("%s.name: %s is a queue of manent", at, r.Name))
		case plugins[r.Name]:
			errs = append(errs, fmt.Errorf("%s.name: %s is the queue of a plugin", at, r.Name))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("%s.name %q is repeated", at, r.Name))
		}
		seen[r.Name] = true
		if len(r.Match.Applications)+len(r.Match.Artifacts)+len(r.Match.Environments) == 0 {
			// An empty match would take every deploy away from the default queue.
			errs = append(errs, fmt.Errorf("%s.match: at least one of applications, artifacts, environments", at))
		}
		for _, a := range r.Match.Applications {
			if err := tenant.CheckAppPattern(a); err != nil {
				errs = append(errs, fmt.Errorf("%s.match.applications: %w", at, err))
			}
		}
		if r.Concurrency < 0 {
			errs = append(errs, fmt.Errorf("%s.concurrency must be >= 0", at))
		}
	}
	for _, q := range c.DBOS.Queues {
		if !seen[q] {
			errs = append(errs, fmt.Errorf("dbos.queues: queue %s is not in queues", q))
		}
	}
	if c.DBOS.Exclusive {
		if len(c.DBOS.Queues) == 0 && len(c.DBOS.Plugins) == 0 {
			errs = append(errs, errors.New("dbos.exclusive requires dbos.queues or dbos.plugins"))
		}
		if len(c.DBOS.Tenants) > 0 {
			errs = append(errs, errors.New("dbos.exclusive and dbos.tenants are alternatives"))
		}
	}
	if len(c.Queues) > 0 && c.DBOS.RegionRouting {
		// The region passes would go to <queue>.<region>, registered only by the region workers
		// that also serve the queue.
		errs = append(errs, errors.New("queues is not supported with dbos.region_routing yet"))
	}
	return errs
}
