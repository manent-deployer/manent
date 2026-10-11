package plugin

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
)

// Registry raccoglie le factory dei plugin disponibili e le istanze attivate da configurazione.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	active    map[string]Deployer
}

// NewRegistry crea un registry vuoto.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}, active: map[string]Deployer{}}
}

// Register rende disponibile una factory con il nome indicato.
func (r *Registry) Register(name string, f Factory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[name]; ok {
		return fmt.Errorf("plugin %q già registrato", name)
	}
	r.factories[name] = f
	return nil
}

// Activate istanzia il plugin con la sua configurazione e lo rende utilizzabile.
func (r *Registry) Activate(name string, cfg map[string]any, logger *slog.Logger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.factories[name]
	if !ok {
		return fmt.Errorf("plugin %q non registrato", name)
	}
	d, err := f(cfg, logger.With("plugin", name))
	if err != nil {
		return fmt.Errorf("attivazione plugin %q: %w", name, err)
	}
	r.active[name] = d
	return nil
}

// Get restituisce un plugin attivo per nome.
func (r *Registry) Get(name string) (Deployer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.active[name]
	return d, ok
}

// Active restituisce i nomi dei plugin attivi in ordine alfabetico (ordine deterministico).
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

// Close rilascia le risorse dei plugin attivi che le possiedono (es. il processo di un plugin
// esterno), cioè quelli che implementano io.Closer.
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
