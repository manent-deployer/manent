package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

// recordStep registra le esecuzioni e può fallire su richiesta.
type recordStep struct {
	name string
	runs *[]string
	fail bool
}

func (s recordStep) Name() string { return s.name }

func (s recordStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	*s.runs = append(*s.runs, s.name)
	if s.fail {
		return nil, errors.New("fallimento simulato")
	}
	// Ogni step vede gli output dei precedenti.
	return pipeline.Output{"seen": strconv.Itoa(len(in.Outputs))}, nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// testRegistry crea registry e planner con tutti gli step obbligatori.
func testRegistry(t *testing.T, steps ...pipeline.Step) (*pipeline.Registry, pipeline.Planner) {
	t.Helper()
	reg, err := pipeline.NewRegistry(steps...)
	if err != nil {
		t.Fatal(err)
	}
	cfgSteps := make([]config.Step, len(steps))
	for i, s := range steps {
		cfgSteps[i] = config.Step{Name: s.Name(), Mode: config.ModeMandatory}
	}
	return reg, pipeline.ConfigPlanner{Steps: cfgSteps, Registry: reg}
}

// workerOpts restituisce le opzioni di un worker di test sul database indicato (testdb.URL).
func workerOpts(databaseURL, executorID string, reg *pipeline.Registry) WorkerOptions {
	return WorkerOptions{
		AppName:     "manent-test",
		DatabaseURL: databaseURL,
		ExecutorID:  executorID,
		Queue:       QueueOptions{Name: "deployments", WorkerConcurrency: 4, PartitionConcurrency: 1},
		Steps:       reg,
		Logger:      discard,

		SchedulePollInterval: 200 * time.Millisecond,
	}
}

// startWorker avvia un worker e ne pianifica l'arresto a fine test.
func startWorker(t *testing.T, opts WorkerOptions) *Worker {
	t.Helper()
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(5 * time.Second) })
	return w
}

// newEngine avvia un singolo worker e restituisce il servizio costruito sul suo contesto.
func newEngine(t *testing.T, steps ...pipeline.Step) *Engine {
	t.Helper()
	reg, planner := testRegistry(t, steps...)
	w := startWorker(t, workerOpts(testdb.URL(t), "w1", reg))
	return NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
}

func testDescriptor(id string) *descriptor.Descriptor {
	return &descriptor.Descriptor{APIVersion: descriptor.APIVersion, ID: id, Application: "app", Environment: "test"}
}

func TestWorkflowEseguePianoInOrdine(t *testing.T) {
	var runs []string
	e := newEngine(t, recordStep{name: "a", runs: &runs}, recordStep{name: "b", runs: &runs})
	ctx := context.Background()

	sub, err := e.Submit(ctx, testDescriptor("deploy-1"))
	if err != nil {
		t.Fatal(err)
	}
	id := sub.ID
	res, err := e.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeploymentID != "deploy-1" || len(res.Steps) != 2 || res.Steps[1].Output["seen"] != "1" {
		t.Fatalf("risultato inatteso: %+v", res)
	}

	// Stesso ID: DBOS non riesegue il workflow (idempotenza).
	if _, err := e.Submit(ctx, testDescriptor("deploy-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, id); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Errorf("step eseguiti %d volte, attese 2: %v", len(runs), runs)
	}

	st, err := e.Status(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "SUCCESS" || st.Result == nil {
		t.Errorf("stato inatteso: %+v", st)
	}
}

func TestWorkflowFallito(t *testing.T) {
	var runs []string
	e := newEngine(t, recordStep{name: "a", runs: &runs, fail: true}, recordStep{name: "b", runs: &runs})
	ctx := context.Background()

	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	id := sub.ID
	if _, err := e.Wait(ctx, id); err == nil {
		t.Fatal("atteso errore")
	}
	if len(runs) != 1 {
		t.Errorf("lo step b non doveva essere eseguito: %v", runs)
	}
	st, err := e.Status(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "ERROR" || st.Error == "" {
		t.Errorf("stato inatteso: %+v", st)
	}
}

// dryStep è uno step che supporta il dry-run e registra quale metodo è stato chiamato.
type dryStep struct {
	recordStep
}

func (s dryStep) DryRun(_ context.Context, _ pipeline.Input) (pipeline.Output, error) {
	*s.runs = append(*s.runs, s.name+":dry")
	return pipeline.Output{"dry": "ok"}, nil
}

func TestWorkflowDryRun(t *testing.T) {
	var runs []string
	e := newEngine(t, dryStep{recordStep{name: "a", runs: &runs}}, recordStep{name: "b", runs: &runs})
	ctx := context.Background()

	d := testDescriptor("deploy-1")
	d.DryRun = true
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if sub.ID != "deploy-1.dry-run" || !sub.DryRun {
		t.Fatalf("submission inattesa: %+v", sub)
	}
	res, err := e.Wait(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Lo step senza DryRun viene saltato, non eseguito.
	if !res.DryRun || len(res.Steps) != 2 || res.Steps[0].Output["dry"] != "ok" || res.Steps[1].Output["dry_run"] != "skipped" {
		t.Fatalf("risultato inatteso: %+v", res)
	}
	if len(runs) != 1 || runs[0] != "a:dry" {
		t.Fatalf("esecuzioni inattese: %v", runs)
	}

	// Il dry-run non consuma l'ID: il deploy reale con lo stesso ID viene eseguito.
	sub, err = e.Submit(ctx, testDescriptor("deploy-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res, err = e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if res.DryRun || len(runs) != 3 || runs[1] != "a" || runs[2] != "b" {
		t.Fatalf("deploy reale non eseguito correttamente: %+v %v", res, runs)
	}
}

// recordNotifier registra gli eventi ricevuti e può fallire su richiesta.
type recordNotifier struct {
	mu     sync.Mutex
	events []callback.Event
	fail   bool
}

func (n *recordNotifier) Notify(_ context.Context, _ *descriptor.Descriptor, _ descriptor.Callback, ev callback.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	if n.fail {
		return errors.New("callback non raggiungibile")
	}
	return nil
}

// summary riduce gli eventi a "tipo/step/stato" per confrontarli.
func (n *recordNotifier) summary() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.events))
	for i, ev := range n.events {
		out[i] = ev.Type + "/" + ev.Step + "/" + ev.Status
	}
	return out
}

func newEngineWithNotifier(t *testing.T, n callback.Notifier, steps ...pipeline.Step) *Engine {
	t.Helper()
	reg, planner := testRegistry(t, steps...)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Notifier = n
	w := startWorker(t, opts)
	return NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
}

func TestCallbackEventi(t *testing.T) {
	var runs []string
	n := &recordNotifier{}
	e := newEngineWithNotifier(t, n, recordStep{name: "a", runs: &runs}, recordStep{name: "b", runs: &runs, fail: true})
	ctx := context.Background()

	d := testDescriptor("deploy-cb")
	d.Callbacks = []descriptor.Callback{
		{URL: "https://ci.example.com/hook"},
		{URL: "https://chat.example.com/hook", Events: []string{descriptor.CallbackEventDeployment}},
	}
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Fatal("atteso errore dallo step b")
	}
	want := []string{
		"deployment//started", "deployment//started",
		"step/a/succeeded",
		"step/b/failed",
		"deployment//failed", "deployment//failed",
	}
	if got := n.summary(); !slices.Equal(got, want) {
		t.Fatalf("eventi inattesi:\n got %v\nwant %v", got, want)
	}
	last := n.events[len(n.events)-1]
	if last.Sequence != 4 || last.ID != "deploy-cb:4" || last.Error == "" || last.Application != "app" {
		t.Errorf("evento finale inatteso: %+v", last)
	}
}

func TestCallbackFallitoNonBloccaDeploy(t *testing.T) {
	var runs []string
	n := &recordNotifier{fail: true}
	e := newEngineWithNotifier(t, n, recordStep{name: "a", runs: &runs})
	ctx := context.Background()

	d := testDescriptor("")
	d.Callbacks = []descriptor.Callback{{URL: "https://ci.example.com/hook"}}
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatalf("il deploy non deve fallire per un callback: %v", err)
	}
	if len(runs) != 1 || len(n.summary()) != 3 {
		t.Errorf("esecuzioni %v, eventi %v", runs, n.summary())
	}

	// Un URL non ammesso dalla policy viene rifiutato già all'accodamento.
	d.Callbacks = []descriptor.Callback{{URL: "http://ci.example.com/hook"}}
	if _, err := e.Submit(ctx, d); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso ErrInvalidRequest, ricevuto %v", err)
	}
}

// permanentStep fallisce con un errore permanente.
type permanentStep struct{ runs *int }

func (permanentStep) Name() string { return "perm" }
func (s permanentStep) Run(context.Context, pipeline.Input) (pipeline.Output, error) {
	*s.runs++
	return nil, pipeline.Permanent(errors.New("policy violata"))
}

func TestErrorePermanenteNonRitentato(t *testing.T) {
	runs := 0
	reg, _ := testRegistry(t, permanentStep{runs: &runs})
	w := startWorker(t, workerOpts(testdb.URL(t), "w1", reg))
	planner := pipeline.ConfigPlanner{Steps: []config.Step{{Name: "perm", Mode: config.ModeMandatory, MaxRetries: 3}}, Registry: reg}
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx := context.Background()
	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Fatal("atteso errore")
	}
	if runs != 1 {
		t.Errorf("uno step con errore permanente non va ritentato: %d esecuzioni", runs)
	}
}

func TestStatusDaClientRiportaLErrore(t *testing.T) {
	var runs []string
	reg, planner := testRegistry(t, recordStep{name: "a", runs: &runs, fail: true})
	db := testdb.URL(t)
	startWorker(t, workerOpts(db, "w1", reg))
	client, err := dbos.NewClient(context.Background(), dbos.ClientConfig{DatabaseURL: db, AppName: "manent-test", Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbos.Shutdown(client, time.Second) })
	e := NewService(ServiceOptions{Client: client, QueueName: "deployments", Planner: planner, Logger: discard})
	sub, err := e.Submit(context.Background(), testDescriptor("err-client"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.Wait(context.Background(), sub.ID)
	st, err := e.Status(context.Background(), sub.ID)
	if err != nil || st.State != "ERROR" || !strings.Contains(st.Error, "fallimento simulato") {
		t.Fatalf("stato da client: %+v %v", st, err)
	}
}
