package engine

import (
	"context"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/testdb"
)

func TestWorkerEAttivita(t *testing.T) {
	var runs []string
	block := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	reg, planner := testRegistry(t, recordStep{name: "a", runs: &runs}, block)
	db := testdb.URL(t)
	opts := workerOpts(db, "w1", reg)
	opts.Registry, opts.Maintenance, opts.Region = openRegistry(t, db), fastMaintenance, "eu"
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{
		Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		Executors: w.Admin(), StaleAfter: time.Minute,
	})
	ctx := context.Background()
	defer close(block.release)

	// d1 keeps running in the blocked step, d2 waits for the environment slot, d3 is scheduled.
	if _, err := e.Submit(ctx, testDescriptor("d1")); err != nil {
		t.Fatal(err)
	}
	<-block.started
	if _, err := e.Submit(ctx, testDescriptor("d2")); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour)
	d3 := testDescriptor("d3")
	d3.Schedule = &descriptor.Schedule{At: &at}
	if _, err := e.Submit(ctx, d3); err != nil {
		t.Fatal(err)
	}
	// A deploy of another application, excluded by the filter.
	other := testDescriptor("x1")
	other.Application, other.Schedule = "altra", &descriptor.Schedule{At: &at}
	if _, err := e.Submit(ctx, other); err != nil {
		t.Fatal(err)
	}

	a, err := e.Activity(ctx, ActivityFilter{Application: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Running) != 1 || a.Running[0].ID != "d1" || a.Running[0].Executor != "w1" || a.Running[0].LastStep != "a" || a.Running[0].StartedAt == nil {
		t.Errorf("in esecuzione: %+v", a.Running)
	}
	if len(a.Queued) != 1 || a.Queued[0].ID != "d2" || a.Queued[0].Executor != "" {
		t.Errorf("in coda: %+v", a.Queued)
	}
	if len(a.Delayed) != 1 || a.Delayed[0].ID != "d3" || a.Delayed[0].ScheduledAt == nil {
		t.Errorf("pianificati: %+v", a.Delayed)
	}

	ws, err := e.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != "w1" || ws[0].State != service.WorkerAlive || ws[0].Region != "eu" || ws[0].HeartbeatAt == nil {
		t.Fatalf("worker: %+v", ws)
	}
	if len(ws[0].Tasks) != 1 || ws[0].Tasks[0].ID != "d1" || ws[0].Tasks[0].Kind != service.TaskDeploy || ws[0].Tasks[0].Application != "app" {
		t.Errorf("lavoro del worker: %+v", ws[0].Tasks)
	}
}

func TestToTaskPassiEGate(t *testing.T) {
	cases := map[string]struct{ id, deployment string }{
		"dep.pass.w1/eu/a#0": {"dep.pass.w1/eu/a#0", "dep"},
		"dep.undo.w1/eu/a":   {"dep.undo.w1/eu/a", "dep"},
		"dep.gate.s1#2.eu":   {"dep.gate.s1#2.eu", "dep"},
	}
	for name, c := range cases {
		if got := parentID(c.id, ".pass.", ".undo.", ".gate."); got != c.deployment {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// Finished workflow counts are read at most every finishedCountsTTL, unfinished ones at every read.
func TestWorkflowCounts(t *testing.T) {
	reg, planner := testRegistry(t, artifactStep{})
	w := startWorker(t, workerOpts(testdb.URL(t), "w1", reg))
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	// A separate Admin: the worker's is also used by maintenance.
	a := NewAdmin(AdminOptions{Client: w.Client(), Logger: discard})
	now := time.Now()
	a.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	count := func(status string) int64 {
		t.Helper()
		cs, err := a.WorkflowCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, c := range cs {
			if c.Workflow == workflowName && c.Status == status {
				n += c.Count
			}
		}
		return n
	}
	deploy := func(id string) {
		t.Helper()
		if _, err := e.Submit(ctx, appVersion("app", "test", "1.0.0", id)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Wait(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	deploy("c1")
	if n := count(StateSuccess); n != 1 {
		t.Fatalf("riusciti: %d", n)
	}
	deploy("c2")
	if n := count(StateSuccess); n != 1 {
		t.Errorf("conteggio dei conclusi riletto prima della scadenza: %d", n)
	}
	now = now.Add(finishedCountsTTL)
	if n := count(StateSuccess); n != 2 {
		t.Errorf("riusciti dopo la scadenza: %d", n)
	}
	// A scheduled (DELAYED) deploy shows up at once.
	d := appVersion("app", "test", "2.0.0", "c3")
	at := time.Now().Add(time.Hour)
	d.Schedule = &descriptor.Schedule{At: &at}
	if _, err := e.Submit(ctx, d); err != nil {
		t.Fatal(err)
	}
	if n := count(StateDelayed); n != 1 {
		t.Errorf("pianificati: %d", n)
	}
}
