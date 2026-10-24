package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
)

// PluginQueueKey is the key of a plugin's configuration (plugins.<name>.queue) that names its
// deploy queue: the deploys of that plugin run only on the workers that list it in
// dbos.plugins, for example a pool with the plugin's runtime (Python for Ansible). manent
// reads the key and does not pass it to the plugin.
const PluginQueueKey = "queue"

// queueRe are the allowed queue names.
var queueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// PluginQueue returns the queue of the plugin, or "" if it runs on the default queue.
func (c *Config) PluginQueue(name string) string {
	q, _ := c.Plugins[name][PluginQueueKey].(string)
	return q
}

// PluginArtifactTypes returns the artifact types configured for the plugin (artifact_types).
func (c *Config) PluginArtifactTypes(name string) []string {
	list, _ := c.Plugins[name]["artifact_types"].([]any)
	var types []string
	for _, t := range list {
		if s, ok := t.(string); ok {
			types = append(types, s)
		}
	}
	return types
}

// ServesPlugin reports whether this process runs the deploys of the plugin: always for a
// plugin without its own queue, and for the others only on a worker that lists it.
func (c *Config) ServesPlugin(name string, worker bool) bool {
	return c.PluginQueue(name) == "" || (worker && slices.Contains(c.DBOS.Plugins, name))
}

// validatePluginQueues checks plugins.<name>.queue and dbos.plugins.
func (c *Config) validatePluginQueues() []error {
	var errs []error
	types := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.Plugins)) {
		raw, ok := c.Plugins[name][PluginQueueKey]
		if !ok {
			continue
		}
		q, ok := raw.(string)
		switch {
		case !ok || !queueRe.MatchString(q):
			errs = append(errs, fmt.Errorf("plugins.%s.queue: invalid queue name %v", name, raw))
			continue
		case q == c.DBOS.Queue || q == c.DBOS.Queue+"-approvals":
			errs = append(errs, fmt.Errorf("plugins.%s.queue: %s is a queue of manent", name, q))
		}
		at := c.PluginArtifactTypes(name)
		if len(at) == 0 {
			errs = append(errs, fmt.Errorf("plugins.%s: a plugin with its own queue needs artifact_types (the deploys are routed by them)", name))
		}
		for _, t := range at {
			if other, dup := types[t]; dup {
				errs = append(errs, fmt.Errorf("plugins.%s: artifact type %s is already routed to the queue of plugin %s", name, t, other))
			}
			types[t] = name
		}
	}
	for _, name := range c.DBOS.Plugins {
		if _, ok := c.Plugins[name]; !ok {
			errs = append(errs, fmt.Errorf("dbos.plugins: plugin %s is not configured", name))
		} else if c.PluginQueue(name) == "" {
			errs = append(errs, fmt.Errorf("dbos.plugins: plugin %s has no queue (plugins.%s.queue)", name, name))
		}
	}
	if len(c.DBOS.Plugins) > 0 && c.DBOS.RegionRouting {
		// The region passes of a plugin's deploys would go to <plugin queue>.<region>, which no
		// worker registers.
		errs = append(errs, errors.New("dbos.plugins is not supported with dbos.region_routing yet"))
	}
	return errs
}
