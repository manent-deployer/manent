package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/manent-deployer/manent/internal/audit"
	"github.com/manent-deployer/manent/internal/auth"
	"github.com/manent-deployer/manent/internal/testdb"
)

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Record(_ context.Context, e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func TestRichiedenteEAudit(t *testing.T) {
	var runs []string
	reg, planner := testRegistry(t, recordStep{name: "a", runs: &runs})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	rec := &memAudit{}
	opts.Audit = rec
	n := &recordNotifier{}
	opts.Notifier = n
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Method: auth.MethodOIDC})
	d := testDescriptor("req-1")
	d.Callbacks = nil
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.RequestedBy != "oidc:alice" || st.Application != "app" || st.Environment != "test" {
		t.Errorf("stato inatteso: %+v", st)
	}
	list, err := e.List(ctx, ListFilter{})
	if err != nil || len(list) != 1 || list[0].RequestedBy != "oidc:alice" || list[0].Environment != "test" {
		t.Errorf("elenco inatteso: %+v %v", list, err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.entries) != 1 {
		t.Fatalf("attesa una voce di audit, trovate %d", len(rec.entries))
	}
	if got := rec.entries[0]; got.Actor != "oidc:alice" || got.Action != ActionDeploymentFinish || got.Outcome != audit.OutcomeSucceeded || got.Deployment != "req-1" {
		t.Errorf("voce di audit inattesa: %+v", got)
	}
}
