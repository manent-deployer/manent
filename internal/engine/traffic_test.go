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
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/steps"
	"github.com/joeg-ita/manent/internal/testdb"
	"github.com/joeg-ita/manent/internal/traffic"
)

// trafficResolver simula la configurazione di ambiente: aggiunge al descrittore la sezione
// traffic, che il descrittore non può definire.
type trafficResolver struct{ cfg *descriptor.TrafficConfig }

func (r trafficResolver) Resolve(_ context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	d.Traffic = r.cfg
	return d, map[string]string{"source": "test"}, nil
}

// fakePool è un pool finto: registra svuotamenti e rientri, ha una connessione residua alla
// prima lettura e i membri in unhealthy restano fuori servizio per le prime letture.
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
	// Il piano contiene solo deploy e activation: drain e restore li inserisce il motore.
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
	// Con web3 non sano lo svuotamento di web1 lascerebbe un solo membro: il primo tentativo
	// viene rimandato e ripetuto (drain@...#0.1).
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

	// Un pool senza rilascio a lotti è rifiutato: svuoterebbe tutti i target insieme.
	d2 := testDescriptor("pool-2")
	d2.Artifact, d2.Deploy.Targets = d.Artifact, d.Deploy.Targets
	sub, err = e.Submit(ctx, d2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "traffic.pool richiede un rilascio a lotti") {
		t.Errorf("pool senza lotti: %v", err)
	}
}

// fakeOps simula le operazioni sul router: registra le chiamate e tiene il peso corrente.
type fakeOps struct {
	mu        sync.Mutex
	log       *waveLog
	weight    int
	hasStable bool
	// drift, se diverso da -1, è il peso letto al gradino indicato da driftAt (modifica esterna).
	driftAt, drift int
	retired        chan struct{}
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

func (f *fakeOps) SetWeight(_ context.Context, _ pipeline.Input, p int) (pipeline.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weight = p
	f.log.add("weight:" + strconv.Itoa(p))
	return pipeline.Output{"weight": strconv.Itoa(p)}, nil
}

func (f *fakeOps) Weight(context.Context, pipeline.Input) (pipeline.Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drift >= 0 && f.weight == f.driftAt {
		f.weight, f.drift = f.drift, -1
	}
	return pipeline.Output{"weight": strconv.Itoa(f.weight)}, nil
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
	close(f.retired)
	return pipeline.Output{}, nil
}

// stageChecker è rosso negli stadi indicati.
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

func newTrafficEngine(t *testing.T) trafficEnv {
	t.Helper()
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	reg, planner := testRegistry(t, countingStep{name: "prepare", n: new(atomic.Int32)},
		waveStep{name: "deploy", worker: "w1", log: l, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl})
	ops := &fakeOps{log: l, hasStable: true, drift: -1, retired: make(chan struct{})}
	ch := &stageChecker{}
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Traffic, opts.Health = ops, ch
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: ch, Traffic: ops})
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
	// Gate dopo ogni gradino, con le tracce per i confronti tra candidata e stabile.
	if len(env.ch.seen) < 3 || env.ch.seen[0].Stage != "traffic@10" || env.ch.seen[0].Track != "b" || env.ch.seen[0].StableTrack != "a" {
		t.Errorf("gate: %+v", env.ch.seen)
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
	if _, err := env.e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "health gate rosso dopo lo stadio traffic@50") || !strings.Contains(err.Error(), "rollback completato") {
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
	// Prima pausa: approvazione dopo i test a peso 0.
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Rollout.Paused != "approvazione dopo i test a peso 0" {
		t.Fatalf("pausa: %+v", st.Rollout)
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	// Seconda pausa: qualcuno ha portato la candidata al 30% fuori da manent.
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
	// Senza versione stabile niente tester né gradini intermedi, e niente da ridimensionare.
	want := []string{"prepare", "deploy:w1:cluster-1", "weight:100", "promote", "activation:w1:cluster-1"}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
}
