package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/labels"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/tenant"
)

// owner returns the tenant that owns the application. Without a registry it returns nil: every
// application is accepted, without owner. With the registry an unregistered application, or
// one of a disabled tenant, is a permanent error; an unreadable registry is temporary.
func (e *Engine) owner(ctx context.Context, application string) (*tenant.Tenant, error) {
	if e.tenants == nil {
		return nil, nil
	}
	t, err := tenant.Owner(ctx, e.tenants, application)
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		return nil, pipeline.Permanent(err)
	case err != nil:
		return nil, fmt.Errorf("reading the tenant registry: %w", err)
	case t.Disabled:
		return nil, pipeline.Permanent(fmt.Errorf("tenant %s, owner of %s, is disabled", t.Name, application))
	}
	return t, nil
}

// ownerRef returns the owner of the application as recorded in requests and statuses; nil
// without a registry or if the owner cannot be read.
func (e *Engine) ownerRef(ctx context.Context, application string) *tenant.Ref {
	t, err := e.owner(ctx, application)
	if err != nil || t == nil {
		return nil
	}
	return t.Ref(application)
}

// appSelector parses an application selector and returns the function that applies it to an
// application, with the tags of the registry (none without registry: only "application" is
// known).
func (e *Engine) appSelector(ctx context.Context, selector string) (func(string) bool, error) {
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
	return func(app string) bool {
		for i := range all {
			if all[i].Owns(app) {
				return s.Matches(all[i].Ref(app).AppLabels())
			}
		}
		return s.Matches(tenant.AppLabels(app, nil, "", nil))
	}, nil
}

// tenantName is the name of the owner, "" without.
func tenantName(r *tenant.Ref) string {
	if r == nil {
		return ""
	}
	return r.Name
}

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
