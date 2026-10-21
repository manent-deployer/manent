package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/tenant"
	"github.com/joeg-ita/manent/internal/testdb"
)

// memDir is a tenant registry in memory.
type memDir []tenant.Tenant

func (d memDir) Tenants(context.Context) ([]tenant.Tenant, error) { return d, nil }

// ownerRegistry: payments owns shop (critical) and billing, with restrictions and its own
// queue; platform owns infra; archive is disabled.
func ownerRegistry() memDir {
	return memDir{
		{Name: "archive", Disabled: true, Applications: map[string]tenant.Application{"old": {}}},
		{Name: "payments", Tags: map[string]string{"division": "commerce"}, Queue: "deployments.payments",
			Applications:    map[string]tenant.Application{"shop": {Tags: map[string]string{"criticality": "high"}}, "billing": {}},
			SecretRefs:      []string{"vault:payments/*"},
			ArtifactSources: []string{"/srv/"}},
		{Name: "platform", Applications: map[string]tenant.Application{"infra": {}}},
	}
}

// ownedDescriptor is a deploy of an application of the registry.
func ownedDescriptor(app, version string) *descriptor.Descriptor {
	d := testDescriptor("")
	d.Application, d.Environment = app, "prod"
	d.Artifact = descriptor.Artifact{Type: "application", Name: app, Version: version, Source: "/srv/" + app + "-" + version + ".tar.gz"}
	return d
}

// ownerEngine starts a worker that serves the default queue and the tenant queues, and an
// engine with the registry.
func ownerEngine(t *testing.T, gov Governance) *Engine {
	t.Helper()
	dir := ownerRegistry()
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.TenantQueues = []QueueOptions{{Name: "deployments.payments"}}
	opts.Governance = gov
	w := startWorker(t, opts)
	return NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Tenants: dir, Governance: gov})
}

func TestProprietarioDelDeploy(t *testing.T) {
	e := ownerEngine(t, nil)
	id := deploy(t, e, alice, ownedDescriptor("shop", "1.0.0"))
	st, err := e.Status(alice, id)
	if err != nil || st.Tenant != "payments" || st.Queue != "deployments.payments" {
		t.Fatalf("il deploy di shop è del tenant payments, sulla sua coda: %+v %v", st, err)
	}
	deploy(t, e, alice, ownedDescriptor("infra", "1.0.0"))
	if inv, err := e.Inventory(alice, InventoryFilter{Tenant: "payments"}); err != nil || len(inv) != 1 || inv[0].Application != "shop" {
		t.Errorf("inventario filtrato per tenant: %+v %v", inv, err)
	}
	if inv, err := e.Inventory(alice, InventoryFilter{Selector: "criticality=high"}); err != nil || len(inv) != 1 || inv[0].Application != "shop" {
		t.Errorf("inventario filtrato per selettore: %+v %v", inv, err)
	}
	if list, err := e.List(alice, ListFilter{Selector: "tenant=platform"}); err != nil || len(list) != 1 || list[0].Application != "infra" {
		t.Errorf("elenco filtrato per selettore: %+v %v", list, err)
	}
	if _, err := e.Submit(alice, ownedDescriptor("unknown", "1.0.0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("applicazione non registrata: %v", err)
	}
	if _, err := e.Submit(alice, ownedDescriptor("old", "1.0.0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("tenant disabilitato: %v", err)
	}
	ts, err := e.Tenants(alice, TenantFilter{Application: "billing"})
	if err != nil || len(ts) != 1 || ts[0].Name != "payments" || ts[0].Applications["shop"].Tags["criticality"] != "high" {
		t.Errorf("proprietario di billing: %+v %v", ts, err)
	}
}

func TestLimitiEProprietaDegliID(t *testing.T) {
	e := ownerEngine(t, nil)
	bad := ownedDescriptor("shop", "1.0.0")
	bad.Secrets = []descriptor.SecretRef{{Name: "db", Ref: "env:DB_PASSWORD"}}
	if _, err := e.Submit(alice, bad); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "secret_refs") {
		t.Errorf("segreto fuori dai pattern del tenant: %v", err)
	}
	bad = ownedDescriptor("shop", "1.0.0")
	bad.Artifact.Source = "https://elsewhere.example.com/shop.tar.gz"
	if _, err := e.Submit(alice, bad); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "artifact_sources") {
		t.Errorf("sorgente fuori dai prefissi del tenant: %v", err)
	}

	mine := ownedDescriptor("infra", "1.0.0")
	mine.ID = "shared-id"
	deploy(t, e, alice, mine)
	theirs := ownedDescriptor("shop", "1.0.0")
	theirs.ID = "shared-id"
	if _, err := e.Submit(alice, theirs); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("ID già usato da un altro tenant: %v", err)
	}
	// The same tenant gets the idempotent resubmission.
	if sub, err := e.Submit(alice, mine); err != nil || sub.ID != "shared-id" {
		t.Errorf("reinvio dello stesso tenant: %+v %v", sub, err)
	}

	// Across tenants a deploy cannot wait for another one, unless it imports its exports.
	after := ownedDescriptor("shop", "1.1.0")
	after.After = []string{"shared-id"}
	if _, err := e.Submit(alice, after); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "another tenant") {
		t.Errorf("after verso un altro tenant: %v", err)
	}

	// A release contains the applications of one tenant.
	r := &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: "mixed", Deploys: []descriptor.ReleaseDeploy{
		{Name: "a", Descriptor: ownedDescriptor("shop", "2.0.0")}, {Name: "b", Descriptor: ownedDescriptor("infra", "2.0.0")},
	}}
	if _, err := e.SubmitRelease(alice, r); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "one tenant") {
		t.Errorf("release tra tenant: %v", err)
	}
}

func TestRegolaPerSelettore(t *testing.T) {
	gov, err := governance.New([]governance.Rule{
		{Name: "critiche", Applications: []string{"*"}, Environments: []string{"prod"}, Selector: "criticality=high",
			Approvals: governance.Approvals{Required: 1, Timeout: time.Minute}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := ownerEngine(t, gov)
	sub, err := e.Submit(alice, ownedDescriptor("shop", "1.0.0"))
	if err != nil || sub.State != StateAwaitingApproval {
		t.Errorf("shop è critica: serve l'approvazione: %+v %v", sub, err)
	}
	if sub, err := e.Submit(alice, ownedDescriptor("billing", "1.0.0")); err != nil || sub.State == StateAwaitingApproval {
		t.Errorf("billing non è critica: %+v %v", sub, err)
	}
}

// A worker dedicated to a tenant (Listen) runs only the deploys of its queue.
func TestWorkerDedicatoAlTenant(t *testing.T) {
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.TenantQueues = []QueueOptions{{Name: "deployments.payments"}}
	opts.Listen = []string{"deployments.payments"}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Tenants: ownerRegistry()})
	deploy(t, e, alice, ownedDescriptor("shop", "1.0.0"))
	sub, err := e.Submit(alice, ownedDescriptor("infra", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if st, err := e.Status(alice, sub.ID); err != nil || st.State != StateEnqueued {
		t.Errorf("il deploy di infra (coda di default) resta in coda: %+v %v", st, err)
	}
}
