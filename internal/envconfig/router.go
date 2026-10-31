package envconfig

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/manent-deployer/manent/internal/glob"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/tenant"
)

// Behaviours of a route when its provider has no configuration for the release.
const (
	// OnMissingFail stops at the route: the deploy has no environment configuration (an error
	// with envconfig.required).
	OnMissingFail = "fail"
	// OnMissingNext moves to the next matching route, for example while migrating applications
	// from one system to another.
	OnMissingNext = "next"
)

// Match selects the releases a route serves; an empty list matches everything.
type Match struct {
	// Applications are application patterns, qualified with the tenant ("payments/*").
	Applications []string
	// Artifacts and Environments are patterns on artifact.name and on the environment.
	Artifacts    []string
	Environments []string
}

func (m Match) matches(k Key) bool {
	return (len(m.Applications) == 0 || tenant.AnyApp(m.Applications, k.Tenant, k.Application)) &&
		(len(m.Artifacts) == 0 || glob.Any(m.Artifacts, k.Artifact)) &&
		(len(m.Environments) == 0 || glob.Any(m.Environments, k.Environment))
}

// Route is a named provider of the router with the releases it serves.
type Route struct {
	Name     string
	Provider Provider
	Match    Match
	// OnMissing is OnMissingFail (default) or OnMissingNext.
	OnMissing string
	// TenantOnly routes serve only the tenants that name them in the registry: Match is not used.
	TenantOnly bool
}

// Router reads every release's configuration from one of several providers: the one the
// release's tenant names in the registry, then the routes whose Match applies, in order. The
// first one with the configuration wins; a missing configuration moves to the next candidate
// only with OnMissingNext, any other error (network, invalid document) never does, so an
// unreachable system cannot silently make the deploy read another configuration.
type Router struct {
	Routes []Route
	// Tenants, if set, is the registry where a tenant can name its route (envconfig).
	Tenants tenant.Directory
}

// Get reads the configuration at the default revision.
func (r *Router) Get(ctx context.Context, k Key) (*Document, error) {
	return r.GetAt(ctx, k, Revision{})
}

// GetAt reads the configuration at the requested revision. A pinned revision is read from the
// provider that recorded it (WithRevision), not from the one the routes would choose now: a
// commit SHA belongs to one repository.
func (r *Router) GetAt(ctx context.Context, k Key, rev Revision) (*Document, error) {
	routes, err := r.candidates(ctx, k)
	if err != nil {
		return nil, err
	}
	if rev.Pinned {
		name := PinnedProvider(ctx)
		i := slices.IndexFunc(r.Routes, func(rt Route) bool { return rt.Name == name })
		if i < 0 {
			return nil, pipeline.Permanent(fmt.Errorf("environment configuration provider %q of the recorded revision is no longer configured (envconfig.providers)", name))
		}
		routes = []*Route{&r.Routes[i]}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("%w: no envconfig provider serves %s/%s/%s/%s", ErrNotFound, k.Tenant, k.Application, k.Artifact, k.Environment)
	}
	var skipped []string
	for i, rt := range routes {
		doc, err := rt.read(ctx, k, rev)
		if errors.Is(err, ErrNotFound) && rt.OnMissing == OnMissingNext && i < len(routes)-1 {
			skipped = append(skipped, rt.Name)
			continue
		}
		if err != nil {
			return nil, err
		}
		doc.Provider, doc.Skipped = rt.Name, skipped
		return doc, nil
	}
	panic("unreachable")
}

// read reads from the route's provider; a requested revision needs a versioned one, a pinned
// one is ignored by the others (it is only recorded by versioned providers).
func (rt *Route) read(ctx context.Context, k Key, rev Revision) (*Document, error) {
	v, ok := rt.Provider.(Versioned)
	switch {
	case ok:
		return v.GetAt(ctx, k, rev)
	case rev.Ref != "" && !rev.Pinned:
		return nil, pipeline.Permanent(fmt.Errorf("%w (provider %s is not versioned)", errRefUnsupported, rt.Name))
	}
	return rt.Provider.Get(ctx, k)
}

// candidates are the routes for the release, without repetitions: the tenant's one first.
func (r *Router) candidates(ctx context.Context, k Key) ([]*Route, error) {
	var out []*Route
	if r.Tenants != nil && k.Tenant != "" && k.Tenant != tenant.Untenanted {
		t, err := tenant.Find(ctx, r.Tenants, k.Tenant)
		switch {
		case errors.Is(err, tenant.ErrNotFound):
		case err != nil:
			return nil, err
		case t.EnvConfig != "":
			i := slices.IndexFunc(r.Routes, func(rt Route) bool { return rt.Name == t.EnvConfig })
			if i < 0 {
				return nil, pipeline.Permanent(fmt.Errorf("tenant %s: envconfig provider %q is not configured (envconfig.providers)", t.Name, t.EnvConfig))
			}
			out = append(out, &r.Routes[i])
		}
	}
	for i := range r.Routes {
		if rt := &r.Routes[i]; !rt.TenantOnly && rt.Match.matches(k) && !slices.Contains(out, rt) {
			out = append(out, rt)
		}
	}
	return out, nil
}
