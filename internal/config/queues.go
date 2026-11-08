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
	// plugins maps the plugins' own queues to the plugin, to name it in the errors.
	plugins := map[string]string{}
	for name := range c.Plugins {
		if q := c.PluginQueue(name); q != "" {
			plugins[q] = name
		}
	}
	seen := map[string]bool{}
	for i, r := range c.Queues {
		at := fmt.Sprintf("queues[%d]", i)
		switch {
		case !queueRe.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s.name: invalid queue name %q: %s", at, r.Name, queueNameRule))
		case r.Name == c.DBOS.Queue || r.Name == c.DBOS.Queue+"-approvals":
			errs = append(errs, fmt.Errorf("%s.name: %s is a queue of manent (dbos.queue or its approvals queue): choose another name; the deploys no entry matches already go to %s, every worker without dbos.exclusive serves it",
				at, r.Name, c.DBOS.Queue))
		case plugins[r.Name] != "":
			p := plugins[r.Name]
			errs = append(errs, fmt.Errorf("%s.name: %s is the queue of a plugin (plugins.%s.queue): choose another name, or remove the entry: the deploys of the artifact_types of plugin %s already go to %s",
				at, r.Name, p, p, r.Name))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("%s.name %q is repeated: a queue has one entry, merge the match patterns of the two entries or rename one", at, r.Name))
		}
		seen[r.Name] = true
		if len(r.Match.Applications)+len(r.Match.Artifacts)+len(r.Match.Environments) == 0 {
			// An empty match would take every deploy away from the default queue.
			errs = append(errs, fmt.Errorf("%s.match: at least one of applications, artifacts, environments: an empty match would send every deploy to %s, add a pattern such as environments: [prod]",
				at, r.Name))
		}
		for _, a := range r.Match.Applications {
			if err := tenant.CheckAppPattern(a); err != nil {
				errs = append(errs, fmt.Errorf("%s.match.applications: %w", at, err))
			}
		}
		if r.Concurrency < 0 {
			errs = append(errs, fmt.Errorf("%s.concurrency must be >= 0 (0 = dbos.global_concurrency)", at))
		}
	}
	for _, q := range c.DBOS.Queues {
		if seen[q] {
			continue
		}
		var hint string
		switch {
		case q == c.DBOS.Queue:
			hint = "it is the default queue, served by every worker without dbos.exclusive: remove it from dbos.queues"
		case plugins[q] != "":
			hint = fmt.Sprintf("it is the queue of plugin %s: move it to dbos.plugins as %s", plugins[q], plugins[q])
		default:
			hint = fmt.Sprintf("add an entry {name: %s, match: {...}} to queues, or remove it from dbos.queues (a tenant's queue needs no listing: dbos.tenants dedicates the worker to a tenant)", q)
		}
		errs = append(errs, fmt.Errorf("dbos.queues: queue %s is not in queues: %s", q, hint))
	}
	if c.DBOS.Exclusive {
		if len(c.DBOS.Queues) == 0 && len(c.DBOS.Plugins) == 0 {
			errs = append(errs, errors.New("dbos.exclusive requires dbos.queues or dbos.plugins: list the queues this worker reserves itself to, or remove dbos.exclusive"))
		}
		if len(c.DBOS.Tenants) > 0 {
			errs = append(errs, errors.New("dbos.exclusive and dbos.tenants are alternatives: dbos.tenants already limits the worker to those tenants' queues (plus dbos.queues and dbos.plugins), remove one of the two"))
		}
	}
	if len(c.Queues) > 0 && c.DBOS.RegionRouting {
		// The region passes would go to <queue>.<region>, registered only by the region workers
		// that also serve the queue.
		errs = append(errs, errors.New("queues is not supported with dbos.region_routing yet: remove queues and dbos.queues (those deploys go back to their tenant's queue or to dbos.queue), or disable dbos.region_routing"))
	}
	return errs
}
