package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/joeg-ita/go-dart/internal/config"
	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/pipeline"
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

func newEngine(t *testing.T, steps ...pipeline.Step) *Engine {
	t.Helper()
	reg, err := pipeline.NewRegistry(steps...)
	if err != nil {
		t.Fatal(err)
	}
	cfgSteps := make([]config.Step, len(steps))
	for i, s := range steps {
		cfgSteps[i] = config.Step{Name: s.Name(), Mode: config.ModeMandatory}
	}
	e, err := New(Options{
		AppName:     "go-dart-test",
		DatabaseURL: "sqlite:" + filepath.Join(t.TempDir(), "dbos.db"),
		QueueName:   "deployments",
		Planner:     pipeline.ConfigPlanner{Steps: cfgSteps, Registry: reg},
		Steps:       reg,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),

		SchedulePollInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Shutdown(5 * time.Second) })
	return e
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
