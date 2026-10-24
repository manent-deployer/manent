package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/cluster"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/testdb"
)

// fakeInspector returns, for every host, the configured version or error.
type fakeInspector struct {
	mu       sync.Mutex
	versions map[string]string
	errs     map[string]error
	calls    int
}

func (f *fakeInspector) set(host, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[host] = version
}

func (f *fakeInspector) InspectTargets(_ context.Context, _ *descriptor.Descriptor, ts []descriptor.Target, _ time.Duration, _ int) ([]string, []error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	vs, errs := make([]string, len(ts)), make([]error, len(ts))
	for i, t := range ts {
		vs[i], errs[i] = f.versions[t.Host], f.errs[t.Host]
	}
	return vs, errs, nil
}

func reconcileDescriptor(id, version string) *descriptor.Descriptor {
	d := testDescriptor(id)
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: version, Source: "/dev/null"}
	d.Deploy.Targets = []descriptor.Target{{Host: "a"}, {Host: "b"}, {Host: "c"}, {Host: "d"}}
	return d
}

// waitTargets waits until the inventory of app/test reports the target states of deployment.
func waitTargets(t *testing.T, e *Engine, deployment string) InventoryEntry {
	t.Helper()
	for range 100 {
		inv, err := e.Inventory(context.Background(), InventoryFilter{Application: "app"})
		if err == nil && len(inv) == 1 && inv[0].Targets != nil && inv[0].Targets.Deployment == deployment {
			return inv[0]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stato dei target del deploy %s non riportato nell'inventario", deployment)
	return InventoryEntry{}
}

func TestRiconciliazioneStatoReale(t *testing.T) {
	db := testdb.URL(t)
	reg, planner := testRegistry(t, countingStep{name: "deploy", n: new(atomic.Int32)})
	ins := &fakeInspector{versions: map[string]string{"a": "2.0.0", "b": "1.9.0", "d": "2.0.0"}, errs: map[string]error{"d": errors.New("connessione rifiutata")}}
	opts := workerOpts(db, "w1", reg)
	opts.Registry = openRegistry(t, db)
	opts.Maintenance = fastMaintenance
	opts.Inspector = ins
	opts.Reconcile = ReconcileOptions{Interval: 300 * time.Millisecond}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, TargetStates: w.Admin()})
	ctx := context.Background()

	sub, err := e.Submit(ctx, reconcileDescriptor("ric-1", "2.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	ie := waitTargets(t, e, sub.ID)
	want := map[string]string{"a": service.TargetInSync, "b": service.TargetDrifted, "c": service.TargetMissing, "d": service.TargetUnknown}
	for _, ts := range ie.Targets.Targets {
		if ts.State != want[ts.Host] {
			t.Errorf("target %s: stato %s, atteso %s", ts.Host, ts.State, want[ts.Host])
		}
	}
	if !ie.OutOfSync || ie.Targets.Version != "2.0.0" || len(ie.Targets.Targets) != 4 {
		t.Errorf("inventario: %+v", ie)
	}

	// A new deploy: the reading of the previous one no longer counts, the next one compares
	// the targets with the new version.
	for _, h := range []string{"a", "b", "c"} {
		ins.set(h, "2.1.0")
	}
	ins.mu.Lock()
	ins.versions["d"], ins.errs = "2.1.0", map[string]error{}
	ins.mu.Unlock()
	sub2, err := e.Submit(ctx, reconcileDescriptor("ric-2", "2.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub2.ID); err != nil {
		t.Fatal(err)
	}
	ie = waitTargets(t, e, sub2.ID)
	if ie.OutOfSync {
		t.Errorf("tutti i target sulla 2.1.0: %+v", ie.Targets)
	}
}

func TestMarkStandby(t *testing.T) {
	ts := []service.TargetState{
		{Host: "a1", Slot: "a", State: service.TargetDrifted},
		{Host: "b1", Slot: "b", State: service.TargetInSync},
		{Host: "x", State: service.TargetDrifted},
	}
	markStandby(ts)
	if ts[0].State != service.TargetStandby || ts[1].State != service.TargetInSync || ts[2].State != service.TargetDrifted {
		t.Errorf("stati: %+v", ts)
	}
	// No slot on the version in service: the environment is out of sync.
	ts = []service.TargetState{{Host: "a1", Slot: "a", State: service.TargetDrifted}, {Host: "b1", Slot: "b", State: service.TargetDrifted}}
	markStandby(ts)
	if ts[0].State != service.TargetDrifted || ts[1].State != service.TargetDrifted {
		t.Errorf("stati senza slot attivo: %+v", ts)
	}
}

func TestMergeTargets(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	recs := []cluster.TargetsRecord{
		{Scope: "", Data: `{"checked_at":"2026-10-17T09:05:00Z","deployment":"d2","version":"2","targets":[{"host":"z","state":"in-sync"}]}`},
		{Scope: "eu-1", Data: `{"checked_at":"2026-10-17T09:00:00Z","deployment":"d2","version":"2","targets":[{"host":"a","state":"drifted"}],"error":"x"}`},
		{Scope: "us-1", Data: `{"checked_at":"2026-10-17T08:00:00Z","deployment":"d1","version":"1","targets":[{"host":"u","state":"in-sync"}]}`},
		// "a" also read earlier by a worker without routing: the most recent reading wins.
		{Scope: "~", Data: `{"checked_at":"2026-10-17T09:01:00Z","deployment":"d2","version":"2","targets":[{"host":"a","state":"in-sync"}]}`},
	}
	st := mergeTargets(recs, "d2")
	if st == nil || len(st.Targets) != 2 || st.Targets[0].Host != "a" || st.Targets[0].State != "in-sync" || !st.CheckedAt.Equal(at) || st.Error != "x" {
		t.Errorf("unione: %+v", st)
	}
	if mergeTargets(recs, "d3") != nil {
		t.Error("nessun record del deploy in esercizio: atteso nil")
	}
}
