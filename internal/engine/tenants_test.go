package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/envconfig"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/tenant"
	"github.com/joeg-ita/manent/internal/testdb"
)

// tenantStep records the tenant deploys (tenant, parameter db_host, targets) and fails for the
// tenants in fail.
type tenantStep struct {
	mu   *sync.Mutex
	log  *[]string
	fail map[string]bool
	wait time.Duration
}

func (tenantStep) Name() string { return "deploy" }

func (s tenantStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	d := in.Descriptor
	var hosts []string
	for _, t := range d.Deploy.Targets {
		hosts = append(hosts, t.Host)
	}
	s.mu.Lock()
	*s.log = append(*s.log, d.Tenant+":"+d.Deploy.Params["db_host"]+":"+strings.Join(hosts, ","))
	s.mu.Unlock()
	time.Sleep(s.wait)
	if s.fail[d.Tenant] {
		return nil, pipeline.Permanent(errors.New("deploy fallito per " + d.Tenant))
	}
	return pipeline.Output{}, nil
}

type tenantEnv struct {
	e    *Engine
	mu   *sync.Mutex
	log  *[]string
	dir  string
	gov  *switchGov
	fail map[string]bool
}

func (te tenantEnv) deploys() []string {
	te.mu.Lock()
	defer te.mu.Unlock()
	return slices.Clone(*te.log)
}

// newTenantEngine starts worker and engine with an environment configuration on files:
// shop/prod is tenanted, shop/test both; three tenants.
func newTenantEngine(t *testing.T, fail map[string]bool) tenantEnv {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := "plugin: test\ntenancy: %s\ntenant_params: {db_host: {pattern: '[a-z0-9.-]+', required: %s, default: %s}}\n" +
		"targets: [{host: shared, labels: {tenant: '*'}}, {host: acme-1, labels: {tenant: acme}}]\n"
	write("shop/prod.yml", sprintf(env, "tenanted", "true", "''")+"params: {dest: '/srv/{tenant}'}\n")
	write("shop/test.yml", sprintf(env, "both", "false", "test-db"))
	write("tenants/acme.yml", "tags: {tier: gold}\napplications: {shop: [test, prod]}\nparams: {shop: {db_host: acme-db}}\n")
	write("tenants/globex.yml", "tags: {tier: silver}\napplications: {shop: [test, prod]}\nparams: {shop: {db_host: globex-db}}\n")
	write("tenants/initech.yml", "tags: {tier: silver}\napplications: {shop: [test, prod]}\nparams: {shop: {db_host: initech-db}}\n")
	prov := &envconfig.FileProvider{PathTemplate: filepath.Join(dir, "{application}", "{environment}.yml"), TenantTemplate: filepath.Join(dir, "tenants", "{tenant}.yml")}
	res := envconfig.Resolver{Provider: prov, Tenants: prov}
	var mu sync.Mutex
	var log []string
	gov := newSwitchGov(t)
	reg, planner := testRegistry(t, artifactStep{}, tenantStep{mu: &mu, log: &log, fail: fail, wait: 100 * time.Millisecond})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Resolver, opts.Governance, opts.ChainPollInterval = res, gov, 200*time.Millisecond
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		Resolver: res, Governance: gov, Tenants: &tenant.Cache{Next: prov, TTL: time.Minute}})
	w.SetService(e)
	return tenantEnv{e: e, mu: &mu, log: &log, dir: dir, gov: gov, fail: fail}
}

func sprintf(f string, args ...string) string {
	for _, a := range args {
		f = strings.Replace(f, "%s", a, 1)
	}
	return f
}

func shop(env, version string) *descriptor.Descriptor {
	d := testDescriptor("")
	d.Application, d.Environment = "shop", env
	d.Artifact = descriptor.Artifact{Type: "application", Name: "shop", Version: version, Source: "/srv/shop-" + version + ".tar.gz"}
	return d
}

func TestDeployDiUnTenant(t *testing.T) {
	te := newTenantEngine(t, nil)
	d := shop("prod", "1.0.0")
	d.Tenant = "acme"
	id := deploy(t, te.e, alice, d)
	if got := te.deploys(); len(got) != 1 || got[0] != "acme:acme-db:shared,acme-1" {
		t.Errorf("deploy del tenant: %v", got)
	}
	st, err := te.e.Status(alice, id)
	if err != nil || st.Tenant != "acme" {
		t.Fatalf("stato: %+v %v", st, err)
	}
	// Untenanted deploy in a tenanted environment and tenant not connected: refused at once.
	if _, err := te.e.Submit(alice, shop("prod", "1.0.0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "requires a tenant") {
		t.Errorf("deploy senza tenant in prod: %v", err)
	}
	d2 := shop("prod", "1.0.0")
	d2.Tenant = "nessuno"
	if _, err := te.e.Submit(alice, d2); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("tenant inesistente: %v", err)
	}
	// Untenanted in a "both" environment: shared targets and default parameter.
	deploy(t, te.e, alice, func() *descriptor.Descriptor { d := shop("test", "1.0.0"); d.Deploy.Params = nil; return d }())
	inv, err := te.e.Inventory(alice, InventoryFilter{Application: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ie := range inv {
		got[ie.Environment+"/"+ie.Tenant] = ie.Current.Version
	}
	if got["prod/acme"] != "1.0.0" || got["test/"] != "1.0.0" || len(got) != 2 {
		t.Errorf("inventario per tenant: %v", got)
	}
	if inv, _ := te.e.Inventory(alice, InventoryFilter{Tenant: "acme"}); len(inv) != 1 {
		t.Errorf("filtro dell'inventario per tenant: %+v", inv)
	}
}

func TestPromozioneObbligatoriaPerTenant(t *testing.T) {
	te := newTenantEngine(t, nil)
	te.gov.set(t, []governance.Rule{{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Requires: []string{"test"}}}, nil)
	acmeTest := shop("test", "2.0.0")
	acmeTest.Tenant = "acme"
	deploy(t, te.e, alice, acmeTest)
	// globex never had 2.0.0 in test: the release of acme does not count.
	g := shop("prod", "2.0.0")
	g.Tenant = "globex"
	if _, err := te.e.Submit(alice, g); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "for tenant globex") {
		t.Fatalf("requires per tenant: %v", err)
	}
	sub, err := te.e.Promote(alice, PromoteRequest{Application: "shop", From: "test", To: "prod", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := te.e.Status(alice, sub.ID); st.Tenant != "acme" || st.Kind != KindPromotion {
		t.Errorf("promozione del tenant: %+v", st)
	}
}

func TestRilascioMultiTenant(t *testing.T) {
	te := newTenantEngine(t, map[string]bool{"globex": true})
	d := shop("prod", "1.0.0")
	d.ID = "shop-100"
	d.Tenants = &descriptor.Tenants{
		Selector:    "tier in (gold, silver)",
		Groups:      []descriptor.TenantGroup{{Name: "pilota", Select: "tier=gold"}, {Name: "resto"}},
		MaxParallel: 1, OnFailure: descriptor.TenantsOnFailureContinue,
	}
	sub, err := te.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	if sub.ID != "shop-100" || len(sub.Tenants) != 3 {
		t.Fatalf("submission: %+v", sub)
	}
	if _, err := te.e.Wait(alice, sub.ID); err == nil || !strings.Contains(err.Error(), "globex") {
		t.Fatalf("il rilascio con un tenant fallito: %v", err)
	}
	st, err := te.e.Status(alice, sub.ID)
	if err != nil || st.TenantsRun == nil || st.TenantsRun.Status != service.TenantsFailed || st.Kind != KindTenants {
		t.Fatalf("stato del rilascio: %+v %v", st, err)
	}
	states := map[string]string{}
	for _, tr := range st.TenantsRun.Tenants {
		states[tr.Tenant] = tr.State
	}
	if states["acme"] != StateSuccess || states["globex"] != StateError || states["initech"] != StateSuccess {
		t.Errorf("stati dei tenant con onFailure continue: %v", states)
	}
	// The pilot group (acme, gold) goes first.
	if got := te.deploys(); len(got) != 3 || !strings.HasPrefix(got[0], "acme:") {
		t.Errorf("ordine dei gruppi: %v", got)
	}
	if child, err := te.e.Status(alice, "shop-100.acme"); err != nil || child.Run != "" && child.Tenant != "acme" {
		t.Errorf("deploy del tenant: %+v %v", child, err)
	}
}

func TestRilascioMultiTenantSiFerma(t *testing.T) {
	te := newTenantEngine(t, map[string]bool{"acme": true})
	d := shop("prod", "1.0.0")
	d.ID = "shop-stop"
	d.Tenants = &descriptor.Tenants{Selector: "tier in (gold, silver)", Groups: []descriptor.TenantGroup{{Name: "pilota", Select: "tier=gold"}, {Name: "resto"}}}
	sub, err := te.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = te.e.Wait(alice, sub.ID)
	st, _ := te.e.Status(alice, sub.ID)
	skipped := 0
	for _, tr := range st.TenantsRun.Tenants {
		if tr.State == service.TenantSkipped {
			skipped++
		}
	}
	if st.TenantsRun.Status != service.TenantsFailed || skipped != 2 || st.TenantsRun.Groups[1].State != "skipped" {
		t.Errorf("onFailure stop: %+v", st.TenantsRun)
	}
	if got := te.deploys(); len(got) != 1 {
		t.Errorf("deploy dopo il fallimento del pilota: %v", got)
	}
}

// tenantDeny denies the deploys of the silver tenants.
type tenantDeny struct{ *switchGov }

func (s *tenantDeny) Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error) {
	dec, err := s.switchGov.Evaluate(ctx, in)
	if in.Tenant != nil && in.Tenant.Tags["tier"] == "silver" && in.Tenant.Name == "initech" {
		dec.Deny = append(dec.Deny, governance.Denial{Code: "contract", Message: "initech stays on 0.x"})
	}
	return dec, err
}

func TestTenantNegatoDallaPolicy(t *testing.T) {
	te := newTenantEngine(t, nil)
	te.e.governance = &tenantDeny{switchGov: te.gov}
	d := shop("prod", "1.0.0")
	d.ID = "shop-deny"
	d.Tenants = &descriptor.Tenants{Selector: "tier=silver"}
	sub, err := te.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Tenants) != 1 || len(sub.Skipped) != 1 || sub.Skipped[0].Tenant != "initech" {
		t.Fatalf("tenant negato escluso: %+v", sub)
	}
	if _, err := te.e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	d.ID, d.Tenants.OnDenied = "shop-reject", descriptor.TenantsOnDeniedReject
	if _, err := te.e.Submit(alice, d); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "initech") {
		t.Errorf("onDenied reject: %v", err)
	}
}

func TestApprovazioneTraGruppiEAnnullamento(t *testing.T) {
	te := newTenantEngine(t, nil)
	d := shop("prod", "1.0.0")
	d.ID = "shop-appr"
	d.Tenants = &descriptor.Tenants{Selector: "tier in (gold, silver)",
		Groups:           []descriptor.TenantGroup{{Name: "pilota", Select: "tier=gold"}, {Name: "resto"}},
		ApprovalsBetween: []string{"pilota"}}
	sub, err := te.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	st := waitDeployState(t, te.e, sub.ID, StateAwaitingApproval)
	if st.Approval == nil || st.TenantsRun.Status != service.TenantsPausedApproval || len(te.deploys()) != 1 {
		t.Fatalf("pausa dopo il pilota: %+v %v", st.TenantsRun, te.deploys())
	}
	if err := te.e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := te.deploys(); len(got) != 3 {
		t.Errorf("tenant rilasciati dopo l'approvazione: %v", got)
	}
	// Rollback of the release: every tenant back to the previous release.
	d2 := shop("prod", "2.0.0")
	d2.ID = "shop-200"
	d2.Tenants = &descriptor.Tenants{Selector: "tier in (gold, silver)"}
	sub2, err := te.e.Submit(alice, d2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Wait(alice, sub2.ID); err != nil {
		t.Fatal(err)
	}
	rb, err := te.e.Rollback(alice, RollbackRequest{Application: "shop", Environment: "prod", Run: "shop-200"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Wait(alice, rb.ID); err != nil {
		t.Fatal(err)
	}
	inv, _ := te.e.Inventory(alice, InventoryFilter{Application: "shop", Environment: "prod"})
	for _, ie := range inv {
		if ie.Current.Version != "1.0.0" || ie.Current.Kind != KindRollback {
			t.Errorf("tenant %s dopo il rollback del rilascio: %s %s", ie.Tenant, ie.Current.Version, ie.Current.Kind)
		}
	}
	// A release cancelled while waiting for its approval leaves the remaining tenants out.
	d3 := shop("prod", "3.0.0")
	d3.ID = "shop-300"
	d3.Tenants = d.Tenants
	sub3, err := te.e.Submit(alice, d3)
	if err != nil {
		t.Fatal(err)
	}
	waitDeployState(t, te.e, sub3.ID, StateAwaitingApproval)
	if err := te.e.Cancel(alice, sub3.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := te.e.Status(alice, sub3.ID); st.State != StateCancelled {
		t.Errorf("rilascio annullato: %s", st.State)
	}
}

func TestPromozioneDiGruppo(t *testing.T) {
	te := newTenantEngine(t, nil)
	for _, tn := range []string{"acme", "globex"} {
		d := shop("test", "4.0.0")
		d.Tenant = tn
		deploy(t, te.e, alice, d)
	}
	sub, err := te.e.Promote(alice, PromoteRequest{Application: "shop", From: "test", To: "prod", Tenants: "tier in (gold, silver)", Redeploy: Redeploy{ID: "promo-4"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Tenants) != 2 || len(sub.Skipped) != 1 || sub.Skipped[0].Tenant != "initech" {
		t.Fatalf("promozione di gruppo: %+v", sub)
	}
	if _, err := te.e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleMultiTenant(t *testing.T) {
	te := newTenantEngine(t, nil)
	d := shop("prod", "5.0.0")
	d.Schedule = &descriptor.Schedule{Cron: "* * * * * *", Name: "notturno"}
	d.Tenants = &descriptor.Tenants{Selector: "tier=silver", MaxParallel: 2}
	sub, err := te.e.Submit(alice, d)
	if err != nil || sub.State != StateScheduled {
		t.Fatalf("schedule: %+v %v", sub, err)
	}
	t.Cleanup(func() { _ = te.e.DeleteSchedule(alice, "notturno") })
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got := te.deploys(); len(got) >= 2 {
			for _, g := range got[:2] {
				if !strings.HasPrefix(g, "globex:") && !strings.HasPrefix(g, "initech:") {
					t.Errorf("tenant inatteso: %v", got)
				}
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("nessuna esecuzione multi-tenant della schedule: %v", te.deploys())
}

func TestAdozioneDiUnAmbientePerCliente(t *testing.T) {
	te := newTenantEngine(t, nil)
	old := shop("test", "6.0.0")
	deploy(t, te.e, alice, old)
	before := len(te.deploys())
	sub, err := te.e.Adopt(alice, AdoptRequest{Application: "shop", From: "test", To: "prod", Tenant: "globex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := te.e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if len(te.deploys()) != before {
		t.Errorf("l'adozione non deve eseguire step: %v", te.deploys())
	}
	inv, _ := te.e.Inventory(alice, InventoryFilter{Application: "shop", Environment: "prod", Tenant: "globex"})
	if len(inv) != 1 || inv[0].Current.Version != "6.0.0" || inv[0].Current.Kind != KindAdoption || inv[0].Current.SHA256 != "sha-6.0.0" {
		t.Fatalf("inventario dopo l'adozione: %+v", inv)
	}
	if _, err := te.e.Adopt(alice, AdoptRequest{Application: "shop", From: "nessuno", To: "prod", Tenant: "globex"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ambiente senza release: %v", err)
	}
}
