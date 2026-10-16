package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

// chaosStep simulates a deploy of variable duration, honors cancellation (like a dying
// process) and measures runs and concurrency per environment.
type chaosStep struct {
	mu        sync.Mutex
	running   map[string]int
	maxEnv    int
	started   map[string]int
	completed map[string]int
}

func (*chaosStep) Name() string { return "work" }

func (s *chaosStep) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	env := in.Descriptor.Environment
	s.mu.Lock()
	s.running[env]++
	s.started[in.DeploymentID]++
	s.maxEnv = max(s.maxEnv, s.running[env])
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running[env]--
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Duration(100+rand.IntN(300)) * time.Millisecond):
	}
	s.mu.Lock()
	s.completed[in.DeploymentID]++
	s.mu.Unlock()
	return pipeline.Output{}, nil
}

// TestCaricoECaos enqueues many deploys on several workers and kills one halfway: all deploys
// must end, each at most twice (the interrupted run and the resumption) and never two at once
// in the same environment. Slow: runs with MANENT_TEST_LOAD=1.
func TestCaricoECaos(t *testing.T) {
	if os.Getenv("MANENT_TEST_LOAD") == "" {
		t.Skip("MANENT_TEST_LOAD non impostata")
	}
	const deploys, envs = 60, 12
	step := &chaosStep{running: map[string]int{}, started: map[string]int{}, completed: map[string]int{}}
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)
	opts := func(id string, crash bool) WorkerOptions {
		o := workerOpts(db, id, reg)
		o.Queue.WorkerConcurrency = 4
		o.Maintenance = fastMaintenance
		o.Maintenance.RecoverAfter = 500 * time.Millisecond
		o.Registry = openRegistry(t, db)
		if crash {
			o.Registry = crashRegistry{o.Registry}
		}
		return o
	}
	// w2 starts first and dequeues deploys; w1 and w3 come later; then w2 dies halfway.
	w2, err := NewWorker(opts("w2", true))
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Launch(); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	ctx := context.Background()
	ids := make([]string, deploys)
	for i := range deploys {
		d := &descriptor.Descriptor{APIVersion: descriptor.APIVersion, ID: fmt.Sprintf("load-%03d", i), Application: "app", Environment: fmt.Sprintf("env-%d", i%envs)}
		sub, err := svc.Submit(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = sub.ID
	}
	w1 := startWorker(t, opts("w1", false))
	startWorker(t, opts("w3", false))
	svc = NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	// Wait until w2 has running deploys.
	for {
		wfs, err := dbos.ListWorkflows(w1.Client(), dbos.WithFilterExecutorIDs("w2"), dbos.WithFilterStatus(dbos.WorkflowStatusPending))
		if err != nil {
			t.Fatal(err)
		}
		if len(wfs) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = w2.Shutdown(10 * time.Millisecond)
	orphans, err := dbos.ListWorkflows(w1.Client(), dbos.WithFilterExecutorIDs("w2"), dbos.WithFilterStatus(dbos.WorkflowStatusPending))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) == 0 {
		t.Fatal("w2 è caduto senza deploy in sospeso")
	}

	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, id := range ids {
		if _, err := svc.Wait(wctx, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	step.mu.Lock()
	defer step.mu.Unlock()
	for _, id := range ids {
		if n := step.completed[id]; n < 1 || n > 2 {
			t.Errorf("%s completato %d volte", id, n)
		}
	}
	restarted := 0
	for _, n := range step.started {
		restarted += n - 1
	}
	t.Logf("deploy rimasti a w2 e ripresi da un altro worker: %d (step interrotti e rieseguiti: %d)", len(orphans), restarted)
	if step.maxEnv > 1 {
		t.Errorf("deploy contemporanei nello stesso ambiente: %d", step.maxEnv)
	}
}
