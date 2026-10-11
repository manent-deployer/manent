package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

// fakeResolver simula una configurazione di ambiente modificabile durante il test.
type fakeResolver struct {
	version atomic.Value // string
	fail    atomic.Bool
}

func newFakeResolver(v string) *fakeResolver {
	r := &fakeResolver{}
	r.version.Store(v)
	return r
}

func (r *fakeResolver) Resolve(_ context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	if r.fail.Load() {
		return d, nil, errors.New("configurazione non disponibile")
	}
	v := r.version.Load().(string)
	d.Deploy.Params = map[string]string{"version": v}
	return d, map[string]string{"source": "fake", "revision": v}, nil
}

// paramStep registra la versione di configurazione vista dagli step.
type paramStep struct {
	mu   sync.Mutex
	seen []string
}

func (*paramStep) Name() string { return "param" }

func (s *paramStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, in.Descriptor.Deploy.Params["version"])
	return pipeline.Output{}, nil
}

func TestConfigLettaAlMomentoDellEsecuzione(t *testing.T) {
	step := &paramStep{}
	reg, planner := testRegistry(t, step)
	resolver := newFakeResolver("v1")
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Resolver = resolver
	w := startWorker(t, opts)
	svc := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx := context.Background()

	at := time.Now().Add(1500 * time.Millisecond)
	sub, err := svc.Submit(ctx, scheduled(descriptor.Schedule{At: &at}, "cfg-1"))
	if err != nil {
		t.Fatal(err)
	}
	// La configurazione cambia dopo l'invio ma prima dell'esecuzione.
	resolver.version.Store("v2")
	res, err := svc.Wait(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].Name != "config" || res.Steps[0].Output["revision"] != "v2" {
		t.Errorf("risultato dello step config inatteso: %+v", res.Steps[0])
	}
	if len(step.seen) != 1 || step.seen[0] != "v2" {
		t.Errorf("gli step dovevano vedere la configurazione v2: %v", step.seen)
	}
}

func TestConfigStabileDopoRecovery(t *testing.T) {
	block := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(block.release) })
	step := &paramStep{}
	reg, planner := testRegistry(t, block, step)
	resolver := newFakeResolver("v1")
	db := testdb.URL(t)
	ctx := context.Background()

	opts := workerOpts(db, "w1", reg)
	opts.Resolver = resolver
	w1, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Launch(); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	sub, err := svc.Submit(ctx, testDescriptor("cfg-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	<-block.started
	_ = w1.Shutdown(100 * time.Millisecond)

	// La configurazione cambia mentre il deploy è interrotto: alla ripresa deve restare v1.
	resolver.version.Store("v2")
	w := startWorker(t, opts)
	svc = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	if _, err := svc.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if len(step.seen) != 1 || step.seen[0] != "v1" {
		t.Errorf("dopo la recovery gli step dovevano vedere ancora v1: %v", step.seen)
	}
}

func TestConfigNonDisponibileFermaIlDeploy(t *testing.T) {
	var n atomic.Int32
	reg, planner := testRegistry(t, countStep{&n})
	resolver := newFakeResolver("v1")
	resolver.fail.Store(true)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Resolver = resolver
	w := startWorker(t, opts)
	svc := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})

	sub, err := svc.Submit(context.Background(), testDescriptor(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Wait(context.Background(), sub.ID); err == nil {
		t.Fatal("atteso errore dello step config")
	}
	if n.Load() != 0 {
		t.Errorf("nessuno step doveva essere eseguito, eseguiti %d", n.Load())
	}
}
