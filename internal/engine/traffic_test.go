package engine

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/steps"
	"github.com/joeg-ita/manent/internal/testdb"
	"github.com/joeg-ita/manent/internal/traffic"
)

// trafficResolver simulates the environment configuration: it adds to the descriptor the
// traffic section, which the descriptor cannot define.
type trafficResolver struct{ cfg *descriptor.TrafficConfig }

func (r trafficResolver) Resolve(_ context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	d.Traffic = r.cfg
	return d, map[string]string{"source": "test"}, nil
}

// fakePool is a fake pool: it records drains and restores, has one remaining connection at
// the first read, and members in unhealthy stay out of service for the first reads.
type fakePool struct {
	log       *waveLog
	hosts     []string
	mu        *sync.Mutex
	reads     *atomic.Int32
	unhealthy map[string]int
}

func (p fakePool) Members(context.Context) ([]traffic.Member, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []traffic.Member
	for _, h := range p.hosts {
		m := traffic.Member{Name: h, State: traffic.StateActive, Healthy: true}
		if p.unhealthy[h] > 0 {
			p.unhealthy[h]--
			m.Healthy = false
		}
		out = append(out, m)
	}
	return out, nil
}

func (p fakePool) Drain(_ context.Context, ms []string) error {
	p.log.add("drain:" + strings.Join(ms, ","))
	return nil
}

func (p fakePool) Connections(_ context.Context, ms []string) (map[string]int, error) {
	out := map[string]int{}
	if p.reads.Add(1)%2 == 1 {
		for _, m := range ms {
			out[m] = 1
		}
	}
	return out, nil
}

func (p fakePool) Restore(_ context.Context, ms []string) error {
	p.log.add("restore:" + strings.Join(ms, ","))
	return nil
}

func (p fakePool) Healthy(_ context.Context, ms []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range ms {
		out[m] = true
	}
	return out, nil
}

func TestPoolSvuotaILotti(t *testing.T) {
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	hosts := []string{"web1", "web2", "web3"}
	fp := fakePool{log: l, hosts: hosts, mu: &sync.Mutex{}, reads: &atomic.Int32{}, unhealthy: map[string]int{"web3": 1}}
	providers := &traffic.Providers{Pools: map[string]traffic.Provider[traffic.PoolFactory]{
		"fake": {New: func(context.Context, traffic.Env) (traffic.Pool, error) { return fp, nil }},
	}}
	pool := steps.Pool{Providers: providers, Logger: discard}
	// The plan contains only deploy and activation: the engine inserts drain and restore.
	_, planner := testRegistry(t, waveStep{name: "deploy", worker: "w1", log: l, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl})
	reg2, err := pipeline.NewRegistry(waveStep{name: "deploy", worker: "w1", log: l, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl},
		steps.Drain{Pool: pool}, steps.Restore{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &descriptor.TrafficConfig{Pool: &descriptor.PoolConfig{Provider: "fake", PollInterval: "1s", MinHealthy: "2"}}
	opts := workerOpts(testdb.URL(t), "w1", reg2)
	opts.Resolver = trafficResolver{cfg: cfg}
	opts.Traffic = steps.Traffic{Pool: pool, Providers: providers, Logger: discard}
	opts.Health = &fakeChecker{red: map[string]bool{}}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})

	d := testDescriptor("pool-1")
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: "2.0.0", Source: "/dev/null"}
	for _, h := range hosts {
		d.Deploy.Targets = append(d.Deploy.Targets, descriptor.Target{Host: h, Labels: map[string]string{"tier": "web"}})
	}
	d.Rollout = &descriptor.Rollout{Waves: []descriptor.Wave{{Name: "tutti", Select: "tier=web"}}}
	ctx := context.Background()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drain:web1", "deploy:w1:web1", "restore:web1", "activation:w1:web1",
		"drain:web2", "deploy:w1:web2", "restore:web2", "activation:w1:web2",
		"drain:web3", "deploy:w1:web3", "restore:web3", "activation:w1:web3",
	}
	if got := l.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
	// With web3 unhealthy, draining web1 would leave a single member: the first attempt is
	// postponed and repeated (drain@...#0.1).
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if !slices.Contains(names, "drain@tutti#0") || !slices.Contains(names, "drain@tutti#0.1") {
		t.Errorf("step: %v", names)
	}
	if res.Steps[1].Output[steps.OutCapacity] != steps.CapacityInsufficient {
		t.Errorf("primo svuotamento non rimandato: %+v", res.Steps[1])
	}
	st, _ := e.Status(ctx, sub.ID)
	if len(st.Rollout.Stages[0].Passes[0].Drained) != 0 {
		t.Errorf("membri ancora svuotati: %+v", st.Rollout.Stages[0].Passes[0])
	}

	// A pool without batched rollout is rejected: it would drain all targets together.
	d2 := testDescriptor("pool-2")
	d2.Artifact, d2.Deploy.Targets = d.Artifact, d.Deploy.Targets
	sub, err = e.Submit(ctx, d2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "traffic.pool requires a batched release") {
		t.Errorf("pool senza lotti: %v", err)
	}
}

// fakeOps simulates the router operations: it records the calls and keeps the current weight.
type fakeOps struct {
	mu  sync.Mutex
	log *waveLog
	// weight is the candidate's share per pass (first target of the pass).
	weight    map[string]int
	hasStable bool
	// drift, if not -1, is the weight read at the step given by driftAt (external change).
	driftAt, drift int
	retired        chan struct{}
	retireOnce     sync.Once
}

func (f *fakeOps) Check(*descriptor.Descriptor) error { return nil }

func (f *fakeOps) PoolStatus(context.Context, pipeline.Input, string) (pipeline.Output, error) {
	return pipeline.Output{"done": "true"}, nil
}

func (f *fakeOps) Prepare(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.log.add("prepare")
	return pipeline.Output{steps.OutMode: steps.ModePlugin, steps.OutRouter: "fake", steps.OutStable: "a", steps.OutCandidate: "b",
		steps.OutHasStable: strconv.FormatBool(f.hasStable), steps.OutState: "{}"}, nil
}

func (f *fakeOps) Testers(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.log.add("testers")
	return pipeline.Output{"tester_header": "x-canary: 1"}, nil
}

func passKey(in pipeline.Input) string { return in.Descriptor.Deploy.Targets[0].Host }

func (f *fakeOps) SetWeight(_ context.Context, in pipeline.Input, p int) (pipeline.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weight[passKey(in)] = p
	f.log.add("weight:" + strconv.Itoa(p))
	return pipeline.Output{"weight": strconv.Itoa(p)}, nil
}

func (f *fakeOps) Weight(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := passKey(in)
	if f.drift >= 0 && f.weight[k] == f.driftAt {
		f.weight[k], f.drift = f.drift, -1
	}
	return pipeline.Output{"weight": strconv.Itoa(f.weight[k])}, nil
}

func (f *fakeOps) Scale(_ context.Context, _ pipeline.Input, p int) (pipeline.Output, error) {
	f.log.add("scale:" + strconv.Itoa(p))
	return pipeline.Output{}, nil
}

func (f *fakeOps) Promote(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.log.add("promote")
	return pipeline.Output{}, nil
}

func (f *fakeOps) Revert(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.log.add("revert")
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted}, nil
}

func (f *fakeOps) Retire(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.log.add("retire")
	f.retireOnce.Do(func() { close(f.retired) })
	return pipeline.Output{}, nil
}

// stageChecker is red in the given stages.
type stageChecker struct {
	mu   sync.Mutex
	red  string
	seen []rollout.GateInput
}

func (c *stageChecker) Check(_ context.Context, in rollout.GateInput) rollout.GateResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, in)
	if in.Stage == c.red {
		return rollout.GateResult{Checks: []rollout.CheckResult{{Kind: rollout.KindMetric, Name: "errori", Detail: "candidata 0.2, stabile 0.01"}}}
	}
	return rollout.GateResult{OK: true}
}

func (c *stageChecker) Supports(*descriptor.Health) error { return nil }

type trafficEnv struct {
	e   *Engine
	log *waveLog
	ops *fakeOps
	ch  *stageChecker
}

func newTrafficEngine(t *testing.T, gov ...Governance) trafficEnv {
	t.Helper()
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	reg, planner := testRegistry(t, countingStep{name: "prepare", n: new(atomic.Int32)},
		waveStep{name: "deploy", worker: "w1", log: l, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl})
	ops := &fakeOps{log: l, hasStable: true, drift: -1, retired: make(chan struct{}), weight: map[string]int{}}
	ch := &stageChecker{}
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Traffic, opts.Health = ops, ch
	var g Governance
	if len(gov) > 0 {
		g = gov[0]
		opts.Governance = g
	}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: ch, Traffic: ops, Governance: g})
	return trafficEnv{e: e, log: l, ops: ops, ch: ch}
}

func trafficDescriptor(id string) *descriptor.Descriptor {
	d := testDescriptor(id)
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: "2.0.0", Source: "/dev/null"}
	d.Deploy.Targets = []descriptor.Target{{Host: "cluster-1"}}
	d.Rollout = &descriptor.Rollout{Strategy: descriptor.StrategyTraffic, Health: &descriptor.Health{},
		Traffic: &descriptor.TrafficRollout{Steps: []int{10, 50, 100}, Testers: &descriptor.Testers{Header: "x-canary: 1"}, ScaleDownDelay: "1s"}}
	return d
}

func TestRilascioPerPercentuale(t *testing.T) {
	env := newTrafficEngine(t)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, trafficDescriptor("canary-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.ops.retired:
	case <-time.After(10 * time.Second):
		t.Fatal("versione sostituita non ridimensionata")
	}
	want := []string{"prepare", "deploy:w1:cluster-1", "testers", "weight:10", "weight:50", "weight:100", "promote", "activation:w1:cluster-1", "retire"}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
	// Gate after every step, with the tracks for candidate vs stable comparisons.
	if len(env.ch.seen) < 3 || env.ch.seen[0].Stage != "traffic@10" || env.ch.seen[0].Track != "b" || env.ch.seen[0].StableTrack != "a" {
		t.Errorf("gate: %+v", env.ch.seen)
	}
	// At 100% the stable version has no traffic: no track to compare with.
	if l := env.ch.seen[len(env.ch.seen)-1]; l.Stage != "traffic@100" || l.Track != "b" || l.StableTrack != "" {
		t.Errorf("gate al 100%%: %+v", l)
	}
	st, err := env.e.Status(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := st.Rollout
	if r == nil || r.Strategy != descriptor.StrategyTraffic || r.Status != service.RolloutSucceeded || r.Traffic.Weight != 100 || !r.Traffic.Promoted || !r.Traffic.Testers {
		t.Fatalf("stato: %+v", r)
	}
	if len(r.Stages) != 3 || r.Stages[0].Name != "10%" || r.Stages[2].State != service.StageSucceeded {
		t.Errorf("stadi: %+v", r.Stages)
	}
}

func TestRilascioPerPercentualeGateRosso(t *testing.T) {
	env := newTrafficEngine(t)
	env.ch.red = "traffic@50"
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, trafficDescriptor("canary-rosso"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "health gate red after stage traffic@50") || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("atteso il ritorno alla versione stabile: %v", err)
	}
	want := []string{"prepare", "deploy:w1:cluster-1", "testers", "weight:10", "weight:50", "revert"}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
	st, _ := env.e.Status(ctx, sub.ID)
	if st.Rollout.Rollback != RollbackCompleted || st.Rollout.Stages[1].State != service.StageFailed || st.Rollout.Stages[0].State != service.StageRolledBack || st.Rollout.Traffic.Weight != 0 {
		t.Errorf("stato: %+v %+v", st.Rollout, st.Rollout.Traffic)
	}
}

func TestRilascioPerPercentualeDerivaEApprovazioni(t *testing.T) {
	env := newTrafficEngine(t)
	env.ops.driftAt, env.ops.drift = 10, 30
	d := trafficDescriptor("canary-deriva")
	d.Rollout.ApprovalsBetween = []string{"0"}
	sub, err := env.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	// First pause: approval after the tests at weight 0.
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Rollout.Paused != "approvazione dopo i test a peso 0" {
		t.Fatalf("pausa: %+v", st.Rollout)
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	// Second pause: someone moved the candidate to 30% outside manent.
	st = waitRollout(t, env.e, sub.ID, service.RolloutPausedDrift)
	if st.State != StateAwaitingApproval || !strings.Contains(st.Rollout.Paused, "30%") {
		t.Fatalf("deriva: %s %+v", st.State, st.Rollout)
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true, Comment: "sovrascrivere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.log.get(); !slices.Contains(got, "weight:50") || !slices.Contains(got, "promote") {
		t.Errorf("esecuzioni: %v", got)
	}
}

func TestPrimoRilascioPerPercentuale(t *testing.T) {
	env := newTrafficEngine(t)
	env.ops.hasStable = false
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, trafficDescriptor("canary-primo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	// Without a stable version no testers, no intermediate steps and nothing to scale down.
	want := []string{"prepare", "deploy:w1:cluster-1", "weight:100", "promote", "activation:w1:cluster-1"}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
}

// With the governance approvals moved into the rollout, the first release, which skips the
// intermediate steps and their approvals, asks them before any traffic.
func TestPrimoRilascioPerPercentualeConApprovazioniNelRollout(t *testing.T) {
	gov, err := governance.New([]governance.Rule{{Applications: []string{"*"}, Environments: []string{"*"},
		Approvals: governance.Approvals{Required: 1, Timeout: time.Minute, When: governance.ApprovalsRollout}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := newTrafficEngine(t, gov)
	env.ops.hasStable = false
	d := trafficDescriptor("canary-primo-gov")
	d.Rollout.ApprovalsBetween = []string{"50"}
	sub, err := env.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Rollout == nil || st.Rollout.Approval == nil || slices.Contains(env.log.get(), "weight:100") {
		t.Fatalf("atteso l'arresto prima del traffico: %+v %v", st.Rollout, env.log.get())
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.log.get(); !slices.Contains(got, "weight:100") {
		t.Errorf("esecuzioni: %v", got)
	}
}

func TestRilascioPerPercentualeAOndate(t *testing.T) {
	env := newTrafficEngine(t)
	d := trafficDescriptor("canary-ondate")
	d.Deploy.Targets = []descriptor.Target{
		{Host: "eu-cluster", Labels: map[string]string{"region": "eu-1", "canary": "true"}},
		{Host: "us-cluster", Labels: map[string]string{"region": "us-1"}},
	}
	d.Rollout.Waves = []descriptor.Wave{{Name: "eu", Select: "canary=true"}, {Name: "resto", Select: "canary!=true"}}
	d.Rollout.Traffic.Steps, d.Rollout.Traffic.Testers = []int{50, 100}, nil
	d.Rollout.ApprovalsBetween = []string{"eu"}
	sub, err := env.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	// After the eu wave the rollout waits for approval before the next cluster.
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Rollout.Paused != "approvazione dopo l'ondata eu" || st.Rollout.Traffic.Pass != "eu" || !st.Rollout.Traffic.Promoted {
		t.Fatalf("pausa: %+v %+v", st.Rollout, st.Rollout.Traffic)
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"prepare", "deploy:w1:eu-cluster", "weight:50", "weight:100", "promote", "activation:w1:eu-cluster",
		"prepare", "deploy:w1:us-cluster", "weight:50", "weight:100", "promote", "activation:w1:us-cluster"}
	got := env.log.get()
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
	st, _ = env.e.Status(context.Background(), sub.ID)
	if len(st.Rollout.Stages) != 4 || st.Rollout.Stages[2].Name != "resto/50%" || st.Rollout.Stages[3].State != service.StageSucceeded {
		t.Errorf("stadi: %+v", st.Rollout.Stages)
	}
	// One scale-down per pass.
	res, _ := env.e.Steps(context.Background(), sub.ID)
	for _, s := range res {
		if strings.HasPrefix(s.Name, "traffic:promote") && s.Name != "traffic:promote@eu" && s.Name != "traffic:promote@resto" {
			t.Errorf("nome dello step: %s", s.Name)
		}
	}
}

func TestRilascioPerPercentualeAOndateRollbackAll(t *testing.T) {
	env := newTrafficEngine(t)
	env.ch.red = "traffic@50@resto"
	d := trafficDescriptor("canary-ondate-rb")
	d.Deploy.Targets = []descriptor.Target{
		{Host: "eu-cluster", Labels: map[string]string{"canary": "true"}},
		{Host: "us-cluster", Labels: map[string]string{"canary": "false"}},
	}
	d.Rollout.Waves = []descriptor.Wave{{Name: "eu", Select: "canary=true"}, {Name: "resto", Select: "canary=false"}}
	d.Rollout.Traffic.Steps, d.Rollout.Traffic.Testers = []int{50, 100}, nil
	sub, err := env.e.Submit(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(context.Background(), sub.ID); err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("atteso il ritorno: %v", err)
	}
	// The return concerns the failed pass first, then the already promoted one.
	var reverts []string
	res, _ := env.e.Steps(context.Background(), sub.ID)
	for _, s := range res {
		if strings.HasPrefix(s.Name, "traffic:revert") {
			reverts = append(reverts, s.Name)
		}
	}
	if !slices.Equal(reverts, []string{"traffic:revert@resto", "traffic:revert@eu"}) {
		t.Errorf("ritorni: %v", reverts)
	}
}

func TestRilascioPerPercentualeRepliche(t *testing.T) {
	env := newTrafficEngine(t)
	d := trafficDescriptor("canary-repliche")
	d.Rollout.Traffic.Replicas, d.Rollout.Traffic.Testers = descriptor.ReplicasProportional, nil
	sub, err := env.e.Submit(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	// The candidate grows before receiving every traffic share.
	want := []string{"prepare", "deploy:w1:cluster-1", "scale:10", "weight:10", "scale:50", "weight:50", "scale:100", "weight:100", "promote"}
	if got := env.log.get(); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
}
