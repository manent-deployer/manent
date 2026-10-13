package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

// crashRegistry simula un crash: l'arresto non viene registrato, come per un nodo che muore.
type crashRegistry struct{ cluster.Registry }

func (crashRegistry) Stopped(context.Context, string, time.Time) error { return nil }

func openRegistry(t *testing.T, db string) *cluster.SQL {
	t.Helper()
	r, err := cluster.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// fastMaintenance sono tempi brevi per i test: un worker è perso dopo 300ms senza heartbeat.
var fastMaintenance = MaintenanceOptions{HeartbeatInterval: 100 * time.Millisecond, StaleAfter: 300 * time.Millisecond}

// crashWorker avvia un worker con un deploy bloccato a metà step e lo fa "morire" senza
// registrare l'arresto. Restituisce l'ID del deploy rimasto in sospeso.
func crashWorker(t *testing.T, opts WorkerOptions, planner pipeline.Planner, step *blockOnceStep) string {
	t.Helper()
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	sub, err := svc.Submit(context.Background(), testDescriptor("perso-1"))
	if err != nil {
		t.Fatal(err)
	}
	<-step.started
	_ = w.Shutdown(100 * time.Millisecond)
	return sub.ID
}

func TestRecoveryAutomaticaDiUnNodoPerso(t *testing.T) {
	step := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(step.release) })
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)

	o1 := workerOpts(db, "w1", reg)
	o1.Registry, o1.Maintenance = crashRegistry{openRegistry(t, db)}, fastMaintenance
	id := crashWorker(t, o1, planner, step)

	// w2 riprende da solo il deploy di w1 quando w1 smette di inviare heartbeat.
	r2 := openRegistry(t, db)
	o2 := workerOpts(db, "w2", reg)
	o2.Registry, o2.Maintenance = r2, fastMaintenance
	o2.Maintenance.RecoverAfter = 500 * time.Millisecond
	w2 := startWorker(t, o2)
	svc := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := svc.Wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	if step.calls.Load() != 2 {
		t.Errorf("attese 2 esecuzioni (interrotta + ripresa), ricevute %d", step.calls.Load())
	}
	es, _ := r2.List(ctx)
	if len(es) != 2 || es[0].ID != "w1" || es[0].RecoveredBy != "executor:w2" {
		t.Fatalf("registro inatteso: %+v", es)
	}

	// Al riavvio w1 si registra di nuovo e non riesegue il deploy già concluso.
	o1.Registry = openRegistry(t, db)
	startWorker(t, o1)
	time.Sleep(500 * time.Millisecond)
	if step.calls.Load() != 2 {
		t.Errorf("deploy rieseguito al riavvio di w1: %d esecuzioni", step.calls.Load())
	}
	if es, _ := r2.List(ctx); es[0].RecoveredBy != "" || es[0].Stale(time.Now(), time.Second) {
		t.Errorf("w1 non registrato di nuovo: %+v", es[0])
	}
}

func TestRecoveryManuale(t *testing.T) {
	step := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(step.release) })
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)

	o1 := workerOpts(db, "w1", reg)
	o1.Registry, o1.Maintenance = crashRegistry{openRegistry(t, db)}, fastMaintenance
	id := crashWorker(t, o1, planner, step)

	o2 := workerOpts(db, "w2", reg)
	o2.Registry, o2.Maintenance = openRegistry(t, db), fastMaintenance
	w2 := startWorker(t, o2)
	adm := w2.Admin()
	ctx := context.Background()

	// w1 ha inviato l'ultimo heartbeat da poco: risulta attivo.
	if _, err := adm.Recover(ctx, RecoverOptions{Executor: "w1", StaleAfter: time.Minute, By: "cli:test"}); !errors.Is(err, ErrExecutorAlive) {
		t.Fatalf("atteso ErrExecutorAlive, ricevuto %v", err)
	}
	if _, err := adm.Recover(ctx, RecoverOptions{Executor: "sconosciuto", StaleAfter: time.Minute}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("executor sconosciuto senza --force: %v", err)
	}
	es, err := adm.Executors(ctx, time.Minute)
	if err != nil || len(es) != 2 || es[0].ID != "w1" || es[0].Pending[w2.Version()] != 1 || !es[1].Alive {
		t.Fatalf("executor inattesi: %+v %v", es, err)
	}

	time.Sleep(400 * time.Millisecond)
	opts := RecoverOptions{Executor: "w1", StaleAfter: 300 * time.Millisecond, By: "cli:test", DryRun: true}
	rep, err := adm.Recover(ctx, opts)
	if err != nil || len(rep.Workflows) != 1 || rep.Workflows[0].ID != id || rep.Workflows[0].Action != RecoverResumed {
		t.Fatalf("dry-run inatteso: %+v %v", rep, err)
	}
	svc := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	if st, _ := svc.Status(ctx, id); st.State != string(dbos.WorkflowStatusPending) {
		t.Fatalf("il dry-run ha modificato il deploy: %s", st.State)
	}

	opts.DryRun = false
	if _, err := adm.Recover(ctx, opts); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := svc.Wait(wctx, id); err != nil {
		t.Fatal(err)
	}
	// Un executor già recuperato non viene recuperato di nuovo.
	if _, err := adm.Recover(ctx, opts); !errors.Is(err, ErrExecutorAlive) {
		t.Fatalf("seconda recovery: atteso rifiuto, ricevuto %v", err)
	}
}

// countingStep conta le esecuzioni.
type countingStep struct {
	name string
	n    *atomic.Int32
}

func (s countingStep) Name() string { return s.name }

func (s countingStep) Run(context.Context, pipeline.Input) (pipeline.Output, error) {
	s.n.Add(1)
	return pipeline.Output{}, nil
}

func TestMigrazioneDeiWorkflowAUnaNuovaVersione(t *testing.T) {
	var first atomic.Int32
	step := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(step.release) })
	reg, planner := testRegistry(t, countingStep{name: "prepare", n: &first}, step)
	db := testdb.URL(t)

	o1 := workerOpts(db, "w1", reg)
	o1.AppVersion, o1.Registry, o1.Maintenance = "v1", crashRegistry{openRegistry(t, db)}, fastMaintenance
	id := crashWorker(t, o1, planner, step)

	// Un deploy accodato dalla versione v1 e mai partito.
	o2 := workerOpts(db, "w2", reg)
	o2.AppVersion, o2.Registry, o2.Maintenance = "v2", openRegistry(t, db), fastMaintenance
	w2 := startWorker(t, o2)
	ctx := context.Background()
	d := testDescriptor("accodato-v1")
	plan, _ := planner.Plan(d)
	req := Request{Descriptor: *d, Plan: plan}
	if _, err := dbos.Enqueue[Result](w2.Client(), "deployments", workflowName, req,
		append(deployOptions(req, d.ID), dbos.WithEnqueueApplicationVersion("v1"))...); err != nil {
		t.Fatal(err)
	}

	// Il worker v2 non esegue workflow v1.
	time.Sleep(time.Second)
	svc := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	if st, _ := svc.Status(ctx, "accodato-v1"); st.State != string(dbos.WorkflowStatusEnqueued) {
		t.Fatalf("workflow v1 eseguito da un worker v2: %s", st.State)
	}

	rep, err := w2.Admin().Recover(ctx, RecoverOptions{FromVersion: "v1", StaleAfter: 300 * time.Millisecond, By: "cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Workflows) != 2 || rep.ToVersion != "v2" {
		t.Fatalf("report inatteso: %+v", rep)
	}
	for _, w := range rep.Workflows {
		if w.Action != RecoverRewound {
			t.Errorf("%s: azione %s (%s)", w.ID, w.Action, w.Note)
		}
		if w.ID == id && w.FromStep == 0 {
			t.Errorf("il deploy interrotto deve ripartire dopo gli step conclusi: %+v", w)
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, wid := range []string{id, "accodato-v1"} {
		if _, err := svc.Wait(wctx, wid); err != nil {
			t.Fatalf("%s: %v", wid, err)
		}
	}
	// "prepare" era già concluso per il deploy interrotto: viene eseguito solo dal secondo.
	if first.Load() != 2 || step.calls.Load() != 3 {
		t.Errorf("esecuzioni inattese: prepare=%d block=%d", first.Load(), step.calls.Load())
	}
}

func TestDrain(t *testing.T) {
	step := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	var n atomic.Int32
	reg, planner := testRegistry(t, step, countingStep{name: "after", n: &n})
	w := startWorker(t, workerOpts(testdb.URL(t), "w1", reg))
	svc := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx := context.Background()
	sub, err := svc.Submit(ctx, testDescriptor("drain-1"))
	if err != nil {
		t.Fatal(err)
	}
	<-step.started

	start := time.Now()
	go func() {
		time.Sleep(500 * time.Millisecond)
		close(step.release)
	}()
	w.Drain(10 * time.Second)
	if time.Since(start) < 400*time.Millisecond {
		t.Fatal("drain concluso con un deploy in esecuzione")
	}
	if st, _ := svc.Status(ctx, sub.ID); st.State != string(dbos.WorkflowStatusSuccess) {
		t.Fatalf("deploy non concluso durante il drain: %s", st.State)
	}
	// Dopo il drain il worker non preleva altri deploy.
	if _, err := svc.Submit(ctx, testDescriptor("drain-2")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if st, _ := svc.Status(ctx, "drain-2"); st.State != string(dbos.WorkflowStatusEnqueued) {
		t.Fatalf("deploy prelevato dopo il drain: %s", st.State)
	}
}

// Un secondo worker con l'executor_id di uno attivo (es. due processi sulla stessa macchina con
// l'hostname come ID) non parte: riprenderebbe i deploy in corso dell'altro. Il primo resta
// registrato come attivo.
func TestExecutorIDDuplicato(t *testing.T) {
	reg, _ := testRegistry(t, artifactStep{})
	db := testdb.URL(t)
	o1 := workerOpts(db, "host1", reg)
	o1.Registry, o1.Maintenance = openRegistry(t, db), fastMaintenance
	startWorker(t, o1)

	o2 := workerOpts(db, "host1", reg)
	o2.Registry, o2.Maintenance = openRegistry(t, db), fastMaintenance
	w2, err := NewWorker(o2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w2.Shutdown(time.Second) })
	var inUse *cluster.InUseError
	if err := w2.Launch(); !errors.As(err, &inUse) {
		t.Fatalf("secondo worker con lo stesso ID avviato: %v", err)
	}
	es, err := o1.Registry.List(context.Background())
	if err != nil || len(es) != 1 || es[0].Stale(time.Now(), fastMaintenance.StaleAfter) {
		t.Fatalf("il primo worker non risulta attivo: %+v %v", es, err)
	}
}
