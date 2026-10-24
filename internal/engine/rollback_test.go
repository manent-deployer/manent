package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/testdb"
)

// compStep is a compensable step that records checkpoints, runs and compensations.
type compStep struct {
	recordStep
	failCompensate bool
	// nothing simulates a compensation with nothing to restore.
	nothing bool
}

func (s compStep) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.Run(ctx, in)
}

func (s compStep) Checkpoint(_ context.Context, _ pipeline.Input) (pipeline.Output, error) {
	*s.runs = append(*s.runs, checkpointPrefix+s.name)
	return pipeline.Output{"previous": "v1-" + s.name}, nil
}

func (s compStep) Compensate(_ context.Context, _ pipeline.Input, cp pipeline.Output) (pipeline.Output, error) {
	*s.runs = append(*s.runs, rollbackPrefix+s.name+"="+cp["previous"])
	if s.failCompensate {
		return nil, errors.New("compensazione fallita")
	}
	if s.nothing {
		return pipeline.Output{pipeline.OutRollback: pipeline.RollbackSkipped}, nil
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted}, nil
}

// fixedPolicy always applies the same rollback decision, or an error.
type fixedPolicy struct {
	rb  *descriptor.Rollback
	err error
}

func (p fixedPolicy) Apply(d descriptor.Descriptor) (descriptor.Descriptor, error) {
	d.Rollback = p.rb
	return d, p.err
}

func (p fixedPolicy) Allowed(string) error { return p.err }

func enabledOn(steps ...string) *descriptor.Rollback {
	on := true
	return &descriptor.Rollback{Enabled: &on, On: steps, Locator: "plugin"}
}

func newRollbackEngine(t *testing.T, pol RollbackPolicy, n *recordNotifier, steps ...pipeline.Step) *Engine {
	t.Helper()
	reg, planner := testRegistry(t, steps...)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Rollback = pol
	if n != nil {
		opts.Notifier = n
	}
	w := startWorker(t, opts)
	return NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
}

func TestRollbackDopoFallimento(t *testing.T) {
	var runs []string
	n := &recordNotifier{}
	e := newRollbackEngine(t, fixedPolicy{rb: enabledOn("c")}, n,
		compStep{recordStep: recordStep{name: "a", runs: &runs}},
		recordStep{name: "b", runs: &runs},
		compStep{recordStep: recordStep{name: "c", runs: &runs, fail: true}},
		recordStep{name: "d", runs: &runs},
	)
	ctx := context.Background()
	d := testDescriptor("rb-1")
	d.Callbacks = []descriptor.Callback{{URL: "https://ci.example.com/hook", Events: []string{descriptor.CallbackEventDeployment}}}
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Wait(ctx, sub.ID)
	if err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("atteso errore con rollback completato, ricevuto %v", err)
	}
	// Compensations in reverse order, failed step included; b is not compensable.
	want := []string{"checkpoint:a", "a", "b", "checkpoint:c", "c", "rollback:c=v1-c", "rollback:a=v1-a"}
	if !slices.Equal(runs, want) {
		t.Fatalf("esecuzioni inattese:\n got %v\nwant %v", runs, want)
	}
	last := n.events[len(n.events)-1]
	if last.Status != "failed" || last.Rollback != RollbackCompleted {
		t.Errorf("evento finale inatteso: %+v", last)
	}
}

func TestRollbackSoloPerGliStepIndicati(t *testing.T) {
	var runs []string
	e := newRollbackEngine(t, fixedPolicy{rb: enabledOn("a")}, nil,
		compStep{recordStep: recordStep{name: "a", runs: &runs}},
		recordStep{name: "b", runs: &runs, fail: true},
	)
	ctx := context.Background()
	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || strings.Contains(err.Error(), "rollback") {
		t.Fatalf("atteso errore senza rollback, ricevuto %v", err)
	}
	if want := []string{"checkpoint:a", "a", "b"}; !slices.Equal(runs, want) {
		t.Fatalf("esecuzioni inattese: %v", runs)
	}
}

func TestRollbackFallitoSiFerma(t *testing.T) {
	var runs []string
	e := newRollbackEngine(t, fixedPolicy{rb: enabledOn("c")}, nil,
		compStep{recordStep: recordStep{name: "a", runs: &runs}},
		compStep{recordStep: recordStep{name: "c", runs: &runs, fail: true}, failCompensate: true},
	)
	ctx := context.Background()
	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("atteso rollback fallito, ricevuto %v", err)
	}
	// a's compensation is not attempted after c's failed one.
	if n := len(runs); runs[n-1] != "rollback:c=v1-c" || slices.Contains(runs, "rollback:a=v1-a") {
		t.Fatalf("esecuzioni inattese: %v", runs)
	}
}

func TestRollbackNienteDaRipristinare(t *testing.T) {
	var runs []string
	e := newRollbackEngine(t, fixedPolicy{rb: enabledOn("a")}, nil,
		compStep{recordStep: recordStep{name: "a", runs: &runs, fail: true}, nothing: true},
	)
	ctx := context.Background()
	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "no release to restore") {
		t.Fatalf("atteso rollback senza release, ricevuto %v", err)
	}
}

func TestRollbackDisattivatoEDryRun(t *testing.T) {
	ctx := context.Background()

	// Rollback disabled: no checkpoint and no compensation.
	var runs []string
	off := false
	e := newRollbackEngine(t, fixedPolicy{rb: &descriptor.Rollback{Enabled: &off}}, nil,
		compStep{recordStep: recordStep{name: "a", runs: &runs, fail: true}},
	)
	sub, err := e.Submit(ctx, testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Fatal("atteso errore")
	}
	if !slices.Equal(runs, []string{"a"}) {
		t.Fatalf("esecuzioni inattese: %v", runs)
	}

	// Dry-run: the checkpoint is verified, but no compensation.
	runs = nil
	e = newRollbackEngine(t, fixedPolicy{rb: enabledOn("a")}, nil,
		compStep{recordStep: recordStep{name: "a", runs: &runs, fail: true}},
	)
	d := testDescriptor("")
	d.DryRun = true
	if sub, err = e.Submit(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Fatal("atteso errore")
	}
	if !slices.Equal(runs, []string{"checkpoint:a", "a"}) {
		t.Fatalf("esecuzioni inattese in dry-run: %v", runs)
	}
}

func TestRollbackNonAmmessoAllAccodamento(t *testing.T) {
	_, planner := testRegistry(t, recordStep{name: "a", runs: new([]string)})
	e := NewService(ServiceOptions{Planner: planner, Rollback: fixedPolicy{err: errors.New("rollback non ammesso")}, Logger: discard})
	if _, err := e.Submit(context.Background(), testDescriptor("")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("atteso ErrInvalidRequest, ricevuto %v", err)
	}
}
