package authz

import (
	"context"
	"fmt"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/tenant"
)

// checkTenants checks the action on every tenant a multi-tenant release selects: the
// selection is resolved here as the engine will resolve it.
func (g *Guard) checkTenants(ctx context.Context, a Action, application, environment string, names []string, selector string, dep *DeploymentInfo) error {
	if g.Registry == nil {
		return fmt.Errorf("%w: tenants: no tenant registry configured", engine.ErrInvalidRequest)
	}
	all, err := g.Registry.Tenants(ctx)
	if err != nil {
		return err
	}
	ts, err := tenant.Select(all, application, environment, names, selector)
	if err != nil {
		return fmt.Errorf("%w: %w", engine.ErrInvalidRequest, err)
	}
	for _, t := range ts {
		if err := g.check(ctx, a, target{application: application, environment: environment, tenant: t.Name, dep: dep}); err != nil {
			return err
		}
	}
	return nil
}

// checkRun checks the action on every tenant of a multi-tenant release (nothing for a deploy).
func (g *Guard) checkRun(ctx context.Context, a Action, st *engine.Status) error {
	if st.TenantsRun == nil {
		return nil
	}
	for _, t := range st.TenantsRun.Tenants {
		if err := g.check(ctx, a, target{application: st.Application, environment: st.Environment, tenant: t.Tenant, deployment: st.ID, dep: statusInfo(st)}); err != nil {
			return err
		}
	}
	return nil
}

// promoteTenants checks the promotion of the selected tenants: read on the source and submit
// on the destination, for every tenant.
func (g *Guard) promoteTenants(ctx context.Context, r engine.PromoteRequest) (*engine.Submission, error) {
	dep := &DeploymentInfo{Kind: engine.KindPromotion, DryRun: r.DryRun, Version: r.Version, RequestedBy: actor(auth.FromContext(ctx))}
	if err := g.checkTenants(ctx, DeploymentRead, r.Application, r.To, nil, r.Tenants, nil); err != nil {
		return nil, err
	}
	if err := g.checkTenants(ctx, submitAction(DeploymentSubmit, r.DryRun), r.Application, r.To, nil, r.Tenants, dep); err != nil {
		return nil, err
	}
	if r.FreezeOverride != "" {
		if err := g.checkTenants(ctx, FreezeOverride, r.Application, r.To, nil, r.Tenants, dep); err != nil {
			return nil, err
		}
	}
	sub, err := g.Next.Promote(ctx, r)
	t := target{application: r.Application, environment: r.To, dryRun: r.DryRun, comment: "tenants " + r.Tenants}
	if sub != nil {
		t.deployment = sub.ID
	}
	g.done(ctx, ActionPromote, t, err)
	return sub, err
}

// Tenants lists only the tenants with at least one readable application/environment.
func (g *Guard) Tenants(ctx context.Context, f engine.TenantFilter) ([]engine.TenantInfo, error) {
	all, err := g.Next.Tenants(ctx, f)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	out := all[:0]
	for _, t := range all {
		if g.readableTenant(ctx, p, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// Tenant requires reading at least one application/environment of the tenant.
func (g *Guard) Tenant(ctx context.Context, name string) (*engine.TenantInfo, error) {
	t, err := g.Next.Tenant(ctx, name)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	if !g.readableTenant(ctx, p, *t) {
		err := fmt.Errorf("%w: %s cannot read tenant %s", engine.ErrForbidden, actor(p), name)
		g.record(ctx, DeploymentRead, target{tenant: name}, audit.OutcomeDenied, err)
		return nil, err
	}
	return t, nil
}

// readableTenant reports whether the identity can read a deploy of the tenant somewhere.
func (g *Guard) readableTenant(ctx context.Context, p *auth.Principal, t engine.TenantInfo) bool {
	ref := &tenant.Ref{Name: t.Name, Tags: t.Tags}
	for app, envs := range t.Applications {
		for _, env := range envs {
			if g.permitted(ctx, p, DeploymentRead, app, "", env, ref) {
				return true
			}
		}
	}
	return false
}
