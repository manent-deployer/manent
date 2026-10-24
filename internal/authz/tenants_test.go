package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/engine"
	"github.com/manent-deployer/manent/internal/tenant"
)

func (f *fakeService) Tenants(context.Context, engine.TenantFilter) ([]engine.TenantInfo, error) {
	return []engine.TenantInfo{
		{Name: "payments", Tags: map[string]string{"division": "commerce"}, Applications: map[string]engine.TenantApplication{"shop": {Tags: map[string]string{"criticality": "high"}}}},
		{Name: "platform", Applications: map[string]engine.TenantApplication{"infra": {}}},
	}, nil
}
func (f *fakeService) Tenant(_ context.Context, name string) (*engine.TenantInfo, error) {
	return &engine.TenantInfo{Name: name, Applications: map[string]engine.TenantApplication{"infra": {}}}, nil
}

// staticDir is a tenant registry in memory.
type staticDir []tenant.Tenant

func (d staticDir) Tenants(context.Context) ([]tenant.Tenant, error) { return d, nil }

// testRegistry: payments owns shop (critical) and billing, platform owns infra.
func testRegistry() staticDir {
	return staticDir{
		{Name: "payments", Tags: map[string]string{"division": "commerce"},
			Applications: map[string]tenant.Application{"shop": {Tags: map[string]string{"criticality": "high"}}, "billing": {}},
			Members:      tenant.Members{Groups: []string{"payments-eng"}}, Admins: tenant.Members{Subjects: []string{"oidc:alice"}}},
		{Name: "platform", Applications: map[string]tenant.Application{"infra": {}}, Members: tenant.Members{Groups: []string{"platform-eng"}}},
	}
}

func as(subject string, groups ...string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Subject: subject, Groups: groups, Method: auth.MethodOIDC})
}

func appDescriptor(app string) *descriptor.Descriptor {
	return &descriptor.Descriptor{Application: app, Environment: "prod", Artifact: descriptor.Artifact{Name: app}}
}

func TestPermessiPerTenantESelettori(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"commerce-support"}, Role: "deployer", Applications: []string{"*/*"}, Environments: []string{"prod"}, Tenants: "division=commerce"},
		{Groups: []string{"sre"}, Role: "approver", Applications: []string{"*/*"}, Environments: []string{"prod"}, Selector: "criticality=high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &Guard{Next: &fakeService{statuses: map[string]engine.Status{}}, Policy: pol, Registry: testRegistry(), Logger: discardLogger()}
	if _, err := g.Submit(as("u", "commerce-support"), appDescriptor("shop")); err != nil {
		t.Errorf("binding per tag del tenant: %v", err)
	}
	if _, err := g.Submit(as("u", "commerce-support"), appDescriptor("infra")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("infra è di un altro tenant: %v", err)
	}
	if !g.allowed(as("u", "sre"), &auth.Principal{Subject: "u", Groups: []string{"sre"}, Method: auth.MethodOIDC}, DeploymentApprove, "payments", "shop", "shop", "prod") {
		t.Error("selettore sui tag dell'applicazione: shop è critica")
	}
	if g.allowed(as("u", "sre"), &auth.Principal{Subject: "u", Groups: []string{"sre"}, Method: auth.MethodOIDC}, DeploymentApprove, "payments", "billing", "billing", "prod") {
		t.Error("billing non è critica")
	}
	// An unregistered application is untenanted: the bindings with tenants or selector do not apply.
	if _, err := g.Submit(as("u", "commerce-support"), appDescriptor("unknown")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("applicazione non registrata: %v", err)
	}
}

func TestRuoliImplicitiDeiTenant(t *testing.T) {
	pol, err := NewPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := &Guard{Next: &fakeService{statuses: map[string]engine.Status{}}, Policy: pol, Registry: testRegistry(), Logger: discardLogger(),
		TenantRoles: TenantRoles{Members: "deployer", Admins: "admin"}}
	if _, err := g.Submit(as("bob", "payments-eng"), appDescriptor("billing")); err != nil {
		t.Errorf("un membro rilascia le applicazioni del suo tenant: %v", err)
	}
	if _, err := g.Submit(as("bob", "payments-eng"), appDescriptor("infra")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("un membro non rilascia quelle di altri tenant: %v", err)
	}
	if err := g.check(as("alice"), FreezeOverride, target{tenant: "payments", application: "shop", artifact: "shop", environment: "prod"}); err != nil {
		t.Errorf("l'admin del tenant ha tutte le azioni sulle sue applicazioni: %v", err)
	}
	if g.permittedAnywhere(as("alice"), &auth.Principal{Subject: "alice", Method: auth.MethodOIDC}, PolicyWrite) {
		t.Error("i ruoli impliciti non danno mai le azioni policy.*")
	}
	ts, err := g.Tenants(as("bob", "payments-eng"), engine.TenantFilter{})
	if err != nil || len(ts) != 1 || ts[0].Name != "payments" {
		t.Errorf("tenant visibili a un membro: %+v %v", ts, err)
	}
	if _, err := g.Tenant(as("bob", "payments-eng"), "platform"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("un tenant senza applicazioni leggibili: %v", err)
	}
}

func TestInventarioTraTenant(t *testing.T) {
	pol, _ := NewPolicy(nil, nil)
	inv := []engine.InventoryEntry{{Tenant: "payments", Application: "shop", Artifact: "shop", Environment: "prod"}, {Tenant: "platform", Application: "infra", Artifact: "infra", Environment: "prod"}}
	svc := &fakeService{statuses: map[string]engine.Status{}, inventory: inv}
	g := &Guard{Next: svc, Policy: pol, Registry: testRegistry(), Logger: discardLogger(), TenantRoles: TenantRoles{Members: "viewer"}}
	got, err := g.Inventory(as("bob", "payments-eng"), engine.InventoryFilter{})
	if err != nil || len(got) != 1 || got[0].Application != "shop" {
		t.Errorf("senza lettura tra tenant solo le proprie applicazioni: %+v %v", got, err)
	}
	g.CrossTenantInventory = true
	svc.inventory = inv
	if got, _ := g.Inventory(as("bob", "payments-eng"), engine.InventoryFilter{}); len(got) != 2 {
		t.Errorf("con la lettura tra tenant tutto l'inventario: %+v", got)
	}
	svc.inventory = inv
	if got, _ := g.Inventory(as("eve", "elsewhere"), engine.InventoryFilter{}); len(got) != 0 {
		t.Errorf("chi non appartiene a un tenant non legge l'inventario degli altri: %+v", got)
	}
}

func TestApplicazioniOmonimeTraTenant(t *testing.T) {
	reg := append(testRegistry(), tenant.Tenant{Name: "retail", Applications: map[string]tenant.Application{"shop": {}}, Members: tenant.Members{Groups: []string{"retail-eng"}}})
	pol, err := NewPolicy(nil, []Binding{
		// Written for the untenanted applications: it never reaches a tenant's shop.
		{Groups: []string{"legacy-ci"}, Role: "deployer", Applications: []string{"shop", "*"}, Environments: []string{"*"}},
		{Groups: []string{"retail-ci"}, Role: "deployer", Applications: []string{"retail/*"}, Environments: []string{"*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &Guard{Next: &fakeService{statuses: map[string]engine.Status{}}, Policy: pol, Registry: reg, Logger: discardLogger(),
		TenantRoles: TenantRoles{Members: "deployer"}}
	shop := func(tn string) *descriptor.Descriptor { d := appDescriptor("shop"); d.Tenant = tn; return d }
	// A member of payments who names retail by mistake: the implicit role covers only payments/shop.
	if _, err := g.Submit(as("bob", "payments-eng"), shop("retail")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("tenant sbagliato con il solo ruolo implicito: %v", err)
	}
	if _, err := g.Submit(as("bob", "payments-eng"), shop("payments")); err != nil {
		t.Errorf("il proprio shop: %v", err)
	}
	if _, err := g.Submit(as("ci", "legacy-ci"), shop("retail")); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("un pattern senza prefisso non copre un tenant: %v", err)
	}
	if _, err := g.Submit(as("ci", "legacy-ci"), appDescriptor("legacy")); err != nil {
		t.Errorf("un pattern senza prefisso copre le applicazioni untenanted: %v", err)
	}
	if _, err := g.Submit(as("ci", "retail-ci"), shop("retail")); err != nil {
		t.Errorf("pattern qualificato: %v", err)
	}
	if _, err := NewPolicy(nil, []Binding{{Groups: []string{"x"}, Role: "viewer", Applications: []string{"a/b/c"}, Environments: []string{"*"}}}); err == nil {
		t.Error("pattern non valido accettato")
	}
}
