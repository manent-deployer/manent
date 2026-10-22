package authz

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/tenant"
)

// tenantBindings are the implicit bindings of the tenants: members and admins receive the roles
// of TenantRoles on the applications of their tenant (selector name=<tenant>).
func (g *Guard) tenantBindings(ctx context.Context) []Binding {
	if g.Registry == nil {
		return nil
	}
	all, err := g.Registry.Tenants(ctx)
	if err != nil {
		return nil
	}
	var out []Binding
	add := func(t tenant.Tenant, m tenant.Members, role string) {
		if role == "" || m.Empty() {
			return
		}
		out = append(out, Binding{Subjects: m.Subjects, Groups: m.Groups, Role: role, Applications: []string{t.Name + "/*"},
			Environments: []string{"*"}, Tenants: tenant.NameLabel + "=" + t.Name})
	}
	for _, t := range all {
		add(t, t.Members, g.TenantRoles.Members)
		add(t, t.Admins, g.TenantRoles.Admins)
	}
	return out
}

// tenantGrants are the grants of the identity from the implicit bindings of the tenants. They
// never contain the static actions (policy.*), decided only by rbac.bindings.
func (g *Guard) tenantGrants(ctx context.Context, p *auth.Principal) []Grant {
	if p == nil {
		return nil
	}
	var out []Grant
	for _, b := range g.tenantBindings(ctx) {
		if !b.Matches(p) {
			continue
		}
		gr, _ := WithoutStatic(Grant{Name: "tenant " + b.Tenants, Role: b.Role, Actions: slices.Clone(g.Policy.Roles[b.Role]),
			Applications: b.Applications, Environments: b.Environments, Tenants: b.Tenants})
		out = append(out, gr)
	}
	return out
}

// memberOfAny reports whether the identity is a member or admin of some tenant.
func (g *Guard) memberOfAny(ctx context.Context, p *auth.Principal) bool {
	if p == nil || g.Registry == nil {
		return false
	}
	all, err := g.Registry.Tenants(ctx)
	if err != nil {
		return false
	}
	for _, t := range all {
		for _, m := range []tenant.Members{t.Members, t.Admins} {
			if (Binding{Subjects: m.Subjects, Groups: m.Groups}).Matches(p) {
				return true
			}
		}
	}
	return false
}

// readableTenant reports whether the identity can read a deploy of an application of the
// tenant, in some environment.
func (g *Guard) readableTenant(ctx context.Context, p *auth.Principal, t engine.TenantInfo) bool {
	if g.Policy.Disabled {
		return true
	}
	now := g.now()
	tags := map[string]string{}
	maps.Copy(tags, t.Tags)
	for _, app := range slices.Sorted(maps.Keys(t.Applications)) {
		ref := &tenant.Ref{Name: t.Name, Tags: tags, Application: app, AppTags: t.Applications[app].Tags}
		for _, gr := range g.allGrants(ctx, p) {
			if gr.Anywhere(DeploymentRead, now) && tenant.AnyApp(gr.Applications, t.Name, app) && len(gr.Environments) > 0 &&
				tenant.Matches(gr.Tenants, ref) && tenant.MatchesApp(gr.Selector, ref) {
				return true
			}
		}
	}
	return false
}

// Tenants lists only the tenants with at least one readable application.
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

// Tenant requires reading at least one application of the tenant.
func (g *Guard) Tenant(ctx context.Context, name string) (*engine.TenantInfo, error) {
	t, err := g.Next.Tenant(ctx, name)
	if err != nil {
		return nil, err
	}
	p := auth.FromContext(ctx)
	if !g.readableTenant(ctx, p, *t) {
		err := fmt.Errorf("%w: %s cannot read tenant %s", engine.ErrForbidden, actor(p), name)
		g.record(ctx, DeploymentRead, target{comment: "tenant " + name}, audit.OutcomeDenied, err)
		return nil, err
	}
	return t, nil
}
