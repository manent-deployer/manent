package plugin

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
)

// Registry collects the factories of the available plugins and the instances activated by
// the configuration.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	active    map[string]Deployer
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}, active: map[string]Deployer{}}
}

// Register makes a factory available under the given name.
func (r *Registry) Register(name string, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[name]; ok {
		return fmt.Errorf("plugin %q already registered", name)
	}
	r.factories[name] = f
	return nil
}

// Activate instantiates the plugin with its configuration and makes it usable.
func (r *Registry) Activate(name string, cfg map[string]any, logger *slog.Logger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.factories[name]
	if !ok {
		return fmt.Errorf("plugin %q not registered", name)
	}
	d, err := f(cfg, logger.With("plugin", name))
	if err != nil {
		return fmt.Errorf("activating plugin %q: %w", name, err)
	}
	r.active[name] = d
	return nil
}

// Get returns an active plugin by name.
func (r *Registry) Get(name string) (Deployer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.active[name]
	return d, ok
}

// Active returns the names of the active plugins in alphabetical order (deterministic order).
func (r *Registry) Active() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.active))
	for n := range r.active {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Close releases the resources of the active plugins that own some (e.g. an external
// plugin's process), that is those implementing io.Closer.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for name, d := range r.active {
		if c, ok := d.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}
