package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

// concurrencyStep measures how many deploys run together, in total and per environment.
type concurrencyStep struct {
	mu      sync.Mutex
	running map[string]int
	maxEnv  int
	maxAll  int
	total   int
	runs    int
	sleep   time.Duration
}

func (*concurrencyStep) Name() string { return "work" }

func (s *concurrencyStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	key := in.Descriptor.Environment
	s.mu.Lock()
	s.running[key]++
	s.total++
	s.runs++
	s.maxEnv = max(s.maxEnv, s.running[key])
	s.maxAll = max(s.maxAll, s.total)
	s.mu.Unlock()

	time.Sleep(s.sleep)

	s.mu.Lock()
	s.running[key]--
	s.total--
	s.mu.Unlock()
	return pipeline.Output{}, nil
}

// blockOnceStep blocks on its first run (ignoring the context, like a process dying halfway
// through a step) and ends at once from the second, i.e. after the recovery.
type blockOnceStep struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (*blockOnceStep) Name() string { return "block" }

func (s *blockOnceStep) Run(context.Context, pipeline.Input) (pipeline.Output, error) {
	if s.calls.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	return pipeline.Output{}, nil
}

func envDescriptor(env string) *descriptor.Descriptor {
	return &descriptor.Descriptor{APIVersion: descriptor.APIVersion, Application: "app", Environment: env}
}

func TestClientNonEseguePrimaDelWorker(t *testing.T) {
	var n atomic.Int32
	reg, planner := testRegistry(t, countStep{&n})
	db := testdb.URL(t)

	// The worker creates the database schema but is not started yet.
	w, err := NewWorker(workerOpts(db, "w1", reg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(5 * time.Second) })

	client, err := dbos.NewClient(context.Background(), dbos.ClientConfig{DatabaseURL: db, AppName: "manent-test", Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbos.Shutdown(client, time.Second) })
	svc := NewService(ServiceOptions{Client: client, QueueName: "deployments", Planner: planner, Logger: discard})

	ctx := context.Background()
	sub, err := svc.Submit(ctx, testDescriptor("client-1"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if st, _ := svc.Status(ctx, sub.ID); st.State != "ENQUEUED" || n.Load() != 0 {
		t.Fatalf("il client non deve eseguire deploy: stato %s, esecuzioni %d", st.State, n.Load())
	}

	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Errorf("attesa 1 esecuzione, ricevute %d", n.Load())
	}
}

func TestDueWorkerSerializzanoPerAmbiente(t *testing.T) {
	step := &concurrencyStep{running: map[string]int{}, sleep: 400 * time.Millisecond}
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)

	// One deploy at a time per worker: two environments in parallel need two workers.
	opts1, opts2 := workerOpts(db, "w1", reg), workerOpts(db, "w2", reg)
	opts1.Queue.WorkerConcurrency, opts2.Queue.WorkerConcurrency = 1, 1
	w1 := startWorker(t, opts1)
	startWorker(t, opts2)
	svc := NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard})

	ctx := context.Background()
	// All prod first and then all test: without partitions the two workers would take two prod
	// ones together (the queue is FIFO).
	var ids []string
	for _, env := range []string{"prod", "test"} {
		for range 3 {
			sub, err := svc.Submit(ctx, envDescriptor(env))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, sub.ID)
		}
	}
	for _, id := range ids {
		if _, err := svc.Wait(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	wfs, err := dbos.ListWorkflows(w1.Client(), dbos.WithFilterWorkflowIDs(ids...))
	if err != nil {
		t.Fatal(err)
	}
	executors := map[string]int{}
	for _, wf := range wfs {
		executors[wf.ExecutorID]++
	}
	step.mu.Lock()
	defer step.mu.Unlock()
	if step.runs != len(ids) {
		t.Errorf("esecuzioni %d, attese %d", step.runs, len(ids))
	}
	if step.maxEnv != 1 {
		t.Errorf("deploy dello stesso ambiente sovrapposti: massimo %d", step.maxEnv)
	}
	if step.maxAll != 2 || len(executors) != 2 {
		t.Errorf("attesi 2 deploy in parallelo su 2 worker: parallelismo %d, executor %v", step.maxAll, executors)
	}
}

func TestRecoverySoloDelProprioExecutor(t *testing.T) {
	step := &blockOnceStep{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(step.release) })
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)
	ctx := context.Background()

	// w1 starts the deploy and "dies" halfway through the step.
	w1, err := NewWorker(workerOpts(db, "w1", reg))
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Launch(); err != nil {
		t.Fatal(err)
	}
	svc1 := NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	sub, err := svc1.Submit(ctx, testDescriptor("recovery-1"))
	if err != nil {
		t.Fatal(err)
	}
	<-step.started
	_ = w1.Shutdown(100 * time.Millisecond)

	// Another worker must not resume w1's deploy, which is still assigned to w1.
	w2 := startWorker(t, workerOpts(db, "w2", reg))
	svc2 := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	time.Sleep(1500 * time.Millisecond)
	if st, _ := svc2.Status(ctx, sub.ID); st.State != "PENDING" || step.calls.Load() != 1 {
		t.Fatalf("w2 non doveva riprendere il deploy: stato %s, esecuzioni %d", st.State, step.calls.Load())
	}

	// On restart with the same ExecutorID, w1 recovers the deploy and completes it.
	startWorker(t, workerOpts(db, "w1", reg))
	if _, err := svc2.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if step.calls.Load() != 2 {
		t.Errorf("attese 2 esecuzioni dello step (interrotta + recovery), ricevute %d", step.calls.Load())
	}
}

// idStep counts the runs of every deploy.
type idStep struct {
	mu   sync.Mutex
	runs map[string]int
}

func (*idStep) Name() string { return "count" }

func (s *idStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[in.DeploymentID]++
	return pipeline.Output{}, nil
}

func TestScheduleConDueWorkerUnDeployPerTick(t *testing.T) {
	step := &idStep{runs: map[string]int{}}
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)
	w1 := startWorker(t, workerOpts(db, "w1", reg))
	startWorker(t, workerOpts(db, "w2", reg))
	svc := NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx := context.Background()

	if _, err := svc.Submit(ctx, scheduled(descriptor.Schedule{Cron: "* * * * * *", Name: "tick"}, "")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	if err := svc.DeleteSchedule(ctx, "tick"); err != nil {
		t.Fatal(err)
	}
	// A tick already started can enqueue its deploy even after the schedule is deleted.
	time.Sleep(2 * time.Second)
	deps, err := svc.List(ctx, ListFilter{Schedule: "tick"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps {
		if _, err := svc.Wait(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	step.mu.Lock()
	defer step.mu.Unlock()
	if len(deps) < 2 {
		t.Fatalf("attesi almeno 2 deploy dai tick, ricevuti %d", len(deps))
	}
	for _, d := range deps {
		if n := step.runs[d.ID]; n != 1 {
			t.Errorf("deploy %s eseguito %d volte", d.ID, n)
		}
	}
	if len(step.runs) != len(deps) {
		t.Errorf("deploy eseguiti %d, elencati %d", len(step.runs), len(deps))
	}
}
