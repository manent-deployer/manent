package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/labels"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/tenant"
)

// owner resolves the tenant of an application: chosen (the descriptor's tenant, --tenant)
// chooses among the tenants that list it (tenant.Resolve). An application no tenant lists is
// Untenanted, unless tenants.untenanted: reject. Ambiguous, unknown, refused or disabled is a
// permanent error; an unreadable registry is temporary.
func (e *Engine) owner(ctx context.Context, application, chosen string) (*tenant.Tenant, error) {
	t, err := tenant.Resolve(ctx, e.tenants, application, chosen, !e.rejectUntenanted)
	switch {
	case errors.Is(err, tenant.ErrNotFound), errors.Is(err, tenant.ErrAmbiguous), errors.Is(err, tenant.ErrDisabled):
		return nil, pipeline.Permanent(err)
	case err != nil:
		return nil, fmt.Errorf("reading the tenant registry: %w", err)
	}
	return t, nil
}

// ownerRef returns the owner of the application as recorded in requests and statuses; the
// Untenanted reference if it cannot be resolved.
func (e *Engine) ownerRef(ctx context.Context, application, chosen string) *tenant.Ref {
	t, err := e.owner(ctx, application, chosen)
	if err != nil {
		return tenant.UntenantedOf(application).Ref(application)
	}
	return t.Ref(application)
}

// tenantRef returns the reference of a tenant by name for one of its applications, with the
// tags of the registry; Untenanted, or a tenant no longer in the registry, without tags.
func (e *Engine) tenantRef(ctx context.Context, name, application string) *tenant.Ref {
	if name != tenant.Untenanted && e.tenants != nil {
		if t, err := tenant.Find(ctx, e.tenants, name); err == nil {
			return t.Ref(application)
		}
	}
	return &tenant.Ref{Name: cmp.Or(name, tenant.Untenanted), Application: application}
}

// appSelector parses an application selector and returns the function that applies it to an
// application of a tenant, with the tags of the registry (Untenanted applications have only
// "application" and "tenant").
func (e *Engine) appSelector(ctx context.Context, selector string) (func(tenantName, app string) bool, error) {
	s, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("%w: selector: %w", ErrInvalidRequest, err)
	}
	var all []tenant.Tenant
	if e.tenants != nil {
		if all, err = e.tenants.Tenants(ctx); err != nil {
			return nil, err
		}
	}
	return func(tenantName, app string) bool {
		for i := range all {
			if all[i].Name == tenantName && all[i].Owns(app) {
				return s.Matches(all[i].Ref(app).AppLabels())
			}
		}
		return s.Matches(tenant.AppLabels(app, nil, cmp.Or(tenantName, tenant.Untenanted), nil))
	}, nil
}

// tenantsOrEmpty is the registry, an empty one without registry.
func (e *Engine) tenantsOrEmpty() tenant.Directory {
	if e.tenants == nil {
		return emptyDirectory{}
	}
	return e.tenants
}

// emptyDirectory is a registry without tenants.
type emptyDirectory struct{}

func (emptyDirectory) Tenants(context.Context) ([]tenant.Tenant, error) { return nil, nil }

// tenantName is the name of the owner, Untenanted for requests without one.
func tenantName(r *tenant.Ref) string {
	if r == nil {
		return tenant.Untenanted
	}
	return r.Name
}

// tenantOf is the tenant of a descriptor, resolved at submission (Untenanted if empty).
func tenantOf(d *descriptor.Descriptor) string { return cmp.Or(d.Tenant, tenant.Untenanted) }

// Tenants implements TenantService.
func (e *Engine) Tenants(ctx context.Context, f TenantFilter) ([]TenantInfo, error) {
	if e.tenants == nil {
		return []TenantInfo{}, nil
	}
	all, err := e.tenants.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	var sel *labels.Selector
	if f.Selector != "" {
		s, err := labels.Parse(f.Selector)
		if err != nil {
			return nil, fmt.Errorf("%w: selector: %w", ErrInvalidRequest, err)
		}
		sel = &s
	}
	out := []TenantInfo{}
	for _, t := range all {
		if (f.Application != "" && !t.Owns(f.Application)) || (sel != nil && !sel.Matches(t.Labels())) {
			continue
		}
		out = append(out, tenantInfo(t))
	}
	return out, nil
}

// Tenant implements TenantService.
func (e *Engine) Tenant(ctx context.Context, name string) (*TenantInfo, error) {
	if e.tenants == nil {
		return nil, fmt.Errorf("%w: tenant %s (no tenant registry)", ErrNotFound, name)
	}
	t, err := tenant.Find(ctx, e.tenants, name)
	if errors.Is(err, tenant.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	ti := tenantInfo(*t)
	return &ti, nil
}

// tenantInfo exposes a tenant: the identities of members and admins and the restrictions are
// left out.
func tenantInfo(t tenant.Tenant) TenantInfo {
	ti := TenantInfo{Name: t.Name, Description: t.Description, Tags: t.Tags, Queue: t.Queue, Concurrency: t.Concurrency,
		Disabled: t.Disabled, Source: t.Source, Revision: t.Revision}
	for _, app := range slices.Sorted(maps.Keys(t.Applications)) {
		if ti.Applications == nil {
			ti.Applications = map[string]TenantApplication{}
		}
		ti.Applications[app] = TenantApplication{Tags: t.Applications[app].Tags}
	}
	return ti
}
