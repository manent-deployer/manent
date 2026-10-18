package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/tenant"
)

func (f *fakeService) Tenants(context.Context, engine.TenantFilter) ([]engine.TenantInfo, error) {
	return []engine.TenantInfo{
		{Name: "acme", Tags: map[string]string{"tier": "gold"}, Applications: map[string][]string{"shop": {"prod"}}},
		{Name: "globex", Tags: map[string]string{"tier": "silver"}, Applications: map[string][]string{"shop": {"prod"}}},
	}, nil
}
func (f *fakeService) Tenant(_ context.Context, name string) (*engine.TenantInfo, error) {
	return &engine.TenantInfo{Name: name, Tags: map[string]string{"tier": "silver"}, Applications: map[string][]string{"shop": {"prod"}}}, nil
}

// staticDir is a tenant registry in memory.
type staticDir []tenant.Tenant

func (d staticDir) Tenants(context.Context) ([]tenant.Tenant, error) { return d, nil }
func (d staticDir) Tenant(_ context.Context, name string) (*tenant.Tenant, error) {
	for _, t := range d {
		if t.Name == name {
			return &t, nil
		}
	}
	return nil, tenant.ErrNotFound
}

func TestPermessiPerTenant(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"support-acme"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"prod"}, Tenants: "name=acme"},
		{Groups: []string{"gold"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"prod"}, Tenants: "tier=gold"},
		{Groups: []string{"ops"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPolicy(nil, []Binding{{Groups: []string{"x"}, Role: "viewer", Applications: []string{"*"}, Environments: []string{"*"}, Tenants: "tier=="}}); err == nil {
		t.Error("selettore non valido accettato")
	}
	dir := staticDir{
		{Name: "acme", Tags: map[string]string{"tier": "silver"}, Applications: map[string][]string{"shop": {"prod"}}},
		{Name: "initech", Tags: map[string]string{"tier": "gold"}, Applications: map[string][]string{"shop": {"prod"}}},
	}
	g := &Guard{Next: &fakeService{statuses: map[string]engine.Status{}}, Policy: pol, Registry: dir, Logger: discardLogger()}
	as := func(groups ...string) context.Context {
		return auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "u", Groups: groups, Method: auth.MethodOIDC})
	}
	d := func(tn string) *descriptor.Descriptor {
		return &descriptor.Descriptor{Application: "shop", Environment: "prod", Tenant: tn}
	}
	if _, err := g.Submit(as("support-acme"), d("acme")); err != nil {
		t.Errorf("supporto di acme su acme: %v", err)
	}
	if _, err := g.Submit(as("support-acme"), d("initech")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("supporto di acme su initech: %v", err)
	}
	if _, err := g.Submit(as("support-acme"), d("")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("un binding con tenants non vale per i deploy senza tenant: %v", err)
	}
	// The tags come from the registry: initech is gold.
	if _, err := g.Submit(as("gold"), d("initech")); err != nil {
		t.Errorf("gruppo gold su initech: %v", err)
	}
	if _, err := g.Submit(as("ops"), d("")); err != nil {
		t.Errorf("binding senza tenants: %v", err)
	}
	multi := d("")
	multi.Tenants = &descriptor.Tenants{Selector: "tier in (gold, silver)"}
	if _, err := g.Submit(as("gold"), multi); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("multi-tenant con un tenant non permesso: %v", err)
	}
	if _, err := g.Submit(as("ops"), multi); err != nil {
		t.Errorf("multi-tenant con permesso su tutti: %v", err)
	}
	ts, err := g.Tenants(as("support-acme"), engine.TenantFilter{})
	if err != nil || len(ts) != 1 || ts[0].Name != "acme" {
		t.Errorf("tenant visibili: %+v %v", ts, err)
	}
}
