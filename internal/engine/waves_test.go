package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/deploylog"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/governance"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/rollout"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/testdb"
)

// waveLog records, in order, runs and compensations of the steps of the wave tests.
type waveLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *waveLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *waveLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// waveStep simulates a step working on targets: it records the hosts (and the worker running
// it), supports checkpoint and compensation and fails on the given hosts.
type waveStep struct {
	name, worker string
	log          *waveLog
	fail         string
	slog         *slog.Logger
}

func (s waveStep) Name() string { return s.name }

func hostsOf(ts []descriptor.Target) string {
	var hs []string
	for _, t := range ts {
		hs = append(hs, t.Host)
	}
	return strings.Join(hs, ",")
}

func (s waveStep) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	h := hostsOf(in.Descriptor.Deploy.Targets)
	s.log.add(s.name + ":" + s.worker + ":" + h)
	s.slog.InfoContext(ctx, "step eseguito", "hosts", h)
	if s.fail != "" && strings.Contains(h, s.fail) {
		return nil, pipeline.Permanent(errors.New("guasto su " + s.fail))
	}
	return pipeline.Output{}, nil
}

func (s waveStep) Checkpoint(context.Context, pipeline.Input) (pipeline.Output, error) {
	return pipeline.Output{"previous": "1.0.0"}, nil
}

func (s waveStep) Compensate(_ context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	s.log.add("undo:" + s.name + ":" + s.worker + ":" + hostsOf(in.Descriptor.Deploy.Targets))
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted}, nil
}

// fakeChecker is a fake health gate: red for the stages touching the given regions.
type fakeChecker struct {
	mu   sync.Mutex
	red  map[string]bool
	seen []string
	// redHosts makes the probes red on these targets.
	redHosts map[string]bool
}

func (c *fakeChecker) Check(_ context.Context, in rollout.GateInput) rollout.GateResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, in.Stage)
	for _, p := range in.Passes {
		if c.red[p.Region] {
			return rollout.GateResult{Checks: []rollout.CheckResult{{Kind: rollout.KindMetric, Name: "error-rate", Region: p.Region, Detail: "valore 0.2 fuori soglia"}}}
		}
		for _, t := range p.Targets {
			if c.redHosts[t.Host] {
				return rollout.GateResult{Checks: []rollout.CheckResult{{Kind: rollout.KindProbe, Name: "health", Host: t.Host, Detail: "stato 503"}}}
			}
		}
	}
	return rollout.GateResult{OK: true}
}

func (c *fakeChecker) Supports(*descriptor.Health) error { return nil }

// waveTargets creates 3 regions × 2 AZs, one node per AZ; the eu-1a node is the canary.
func waveTargets() []descriptor.Target {
	var ts []descriptor.Target
	for _, reg := range []string{"eu-1", "eu-2", "us-1"} {
		for _, az := range []string{"a", "b"} {
			l := map[string]string{"region": reg, "az": reg + az}
			if reg == "eu-1" && az == "a" {
				l["canary"] = "true"
			}
			ts = append(ts, descriptor.Target{Host: reg + az, Labels: l})
		}
	}
	return ts
}

func waveDescriptor(id string) *descriptor.Descriptor {
	d := testDescriptor(id)
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: "2.0.0", Source: "/dev/null"}
	d.Deploy.Targets = waveTargets()
	d.Rollout = &descriptor.Rollout{
		Regions: []string{"eu-1", "eu-2", "us-1"},
		Waves: []descriptor.Wave{
			{Name: "canary", Select: "canary=true"},
			{Name: "eu-1", Select: "region=eu-1, canary!=true", By: "az"},
			{Name: "resto", Select: "region in (eu-2, us-1)", By: "az", ParallelRegions: 2},
		},
		Health: &descriptor.Health{},
	}
	return d
}

type waveEnv struct {
	e       *Engine
	log     *waveLog
	checker *fakeChecker
}

// newWaveEngine starts a worker with the steps prepare (once), deploy and activation (per pass).
func newWaveEngine(t *testing.T, fail string, gov Governance) waveEnv {
	t.Helper()
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	reg, planner := testRegistry(t, countingStep{name: "prepare", n: new(atomic.Int32)},
		waveStep{name: "deploy", worker: "w1", log: l, fail: fail, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl})
	ch := &fakeChecker{red: map[string]bool{}}
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Health = ch
	opts.Governance = gov
	// The worker writes to the deploy log too (red health gate reads).
	opts.Logger = sl
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: ch, Governance: gov})
	return waveEnv{e: e, log: l, checker: ch}
}

func TestRolloutAOndate(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"deploy:w1:eu-1a", "activation:w1:eu-1a", // canary
		"deploy:w1:eu-1b", "activation:w1:eu-1b", // eu-1, one AZ at a time
		"deploy:w1:eu-2a", "activation:w1:eu-2a", "deploy:w1:us-1a", "activation:w1:us-1a", // eu-2 and us-1 together, AZ a
		"deploy:w1:eu-2b", "activation:w1:eu-2b", "deploy:w1:us-1b", "activation:w1:us-1b",
	}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Fatalf("esecuzioni:\n %v\natteso\n %v", got, want)
	}
	if got := env.checker.seen; len(got) != 4 || got[0] != "canary" || got[3] != "resto/eu-2b+us-1b" {
		t.Errorf("health gate letti: %v", got)
	}
	st, err := env.e.Status(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rollout == nil || st.Rollout.Status != service.RolloutSucceeded || st.RolloutProgress != "4/4" {
		t.Fatalf("stato del rollout: %+v %q", st.Rollout, st.RolloutProgress)
	}
	if s := st.Rollout.Stages[2]; s.Name != "resto/eu-2a+us-1a" || len(s.Passes) != 2 || s.Passes[1].Hosts[0] != "us-1a" {
		t.Errorf("stadio inatteso: %+v", s)
	}
	// The logs of the pass steps reach the deploy log with the pass name.
	p, _ := env.e.Logs(ctx, sub.ID, 0)
	if len(p.Lines) == 0 || p.Lines[0].Step != "deploy@canary#0" {
		t.Errorf("log: %+v", p.Lines)
	}
}

func TestRolloutGateRossoFerma(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	env.checker.red["us-1"] = true
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-stop"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "health gate red after stage resto/eu-2a+us-1a") {
		t.Fatalf("atteso stop al gate, ricevuto %v", err)
	}
	for _, c := range env.log.get() {
		if strings.HasSuffix(c, "b") && strings.Contains(c, "us-1") || strings.HasPrefix(c, "undo") {
			t.Errorf("eseguito dopo lo stop: %s", c)
		}
	}
	st, _ := env.e.Status(ctx, sub.ID)
	if st.Rollout.Status != service.RolloutFailed || st.Rollout.Stages[2].State != service.StageFailed || st.Rollout.Stages[3].State != service.StagePending || st.RolloutProgress != "2/4" {
		t.Fatalf("stato: %+v %q", st.Rollout, st.RolloutProgress)
	}
	if !strings.Contains(st.Rollout.Stages[2].Gate, "error-rate (us-1): valore 0.2") {
		t.Errorf("gate: %q", st.Rollout.Stages[2].Gate)
	}
	// The red read is in the deploy log, under the gate step.
	p, _ := env.e.Logs(ctx, sub.ID, 0)
	if !slices.ContainsFunc(p.Lines, func(l service.LogLine) bool {
		return l.Level == "WARN" && l.Message == "health gate red" && strings.HasPrefix(l.Step, "gate@resto/eu-2a+us-1a#") && strings.Contains(l.Attrs["reason"], "error-rate (us-1)")
	}) {
		t.Errorf("gate rosso non nel log: %+v", p.Lines)
	}
	// The inventory flags the misaligned environment.
	inv, _ := env.e.Inventory(ctx, InventoryFilter{})
	if len(inv) != 1 || !inv[0].Drift {
		t.Errorf("inventario senza drift: %+v", inv)
	}
}

func TestRolloutRollbackAll(t *testing.T) {
	env := newWaveEngine(t, "us-1a", nil)
	ctx := context.Background()
	d := waveDescriptor("ondate-rb")
	on := true
	d.Rollback = &descriptor.Rollback{Enabled: &on}
	d.Rollout.OnFailure = descriptor.OnFailureRollbackAll
	sub, err := env.e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.e.Wait(ctx, sub.ID)
	if err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("atteso rollback completato, ricevuto %v", err)
	}
	var undo []string
	for _, c := range env.log.get() {
		if strings.HasPrefix(c, "undo:") {
			undo = append(undo, strings.TrimPrefix(c, "undo:"))
		}
	}
	// In reverse order: the failed stage (us-1a failed, eu-2a), then eu-1b, then the canary.
	want := []string{"deploy:w1:us-1a", "activation:w1:eu-2a", "deploy:w1:eu-2a", "activation:w1:eu-1b", "deploy:w1:eu-1b", "activation:w1:eu-1a", "deploy:w1:eu-1a"}
	if !slices.Equal(undo, want) {
		t.Fatalf("compensazioni:\n %v\natteso\n %v", undo, want)
	}
	st, _ := env.e.Status(ctx, sub.ID)
	if st.Rollout.Rollback != RollbackCompleted || st.Rollout.Stages[0].State != service.StageRolledBack {
		t.Errorf("stato: %+v", st.Rollout)
	}
	// A rollback requested without rollback enabled is rejected at submission.
	d2 := waveDescriptor("ondate-rb-2")
	d2.Rollout.OnFailure = descriptor.OnFailureRollbackWave
	if _, err := env.e.Submit(ctx, d2); err == nil {
		// Without a resolver the check happens in the worker: the deploy fails at once.
		if _, err := env.e.Wait(ctx, "ondate-rb-2"); err == nil || !strings.Contains(err.Error(), "requires rollback enabled") {
			t.Errorf("rollback non attivo: %v", err)
		}
	}
}

func TestRolloutApprovazioneTraLeOndate(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	d := waveDescriptor("ondate-appr")
	d.Rollout.ApprovalsBetween = []string{"canary"}
	sub, err := env.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Approval == nil || st.Rollout.Paused != "approval after the wave canary" || len(env.log.get()) != 2 {
		t.Fatalf("pausa inattesa: %+v %v", st.Rollout, env.log.get())
	}
	// Self-approval is discarded by the workflow; another identity's approval unblocks it.
	if err := env.e.Decide(alice, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if st, _ := env.e.Status(ctx, sub.ID); st.State != StateAwaitingApproval {
		t.Fatalf("auto-approvazione accettata: %s", st.State)
	}
	if err := env.e.Decide(bob, sub.ID, DecisionRequest{Approve: true, Comment: "canary ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(env.log.get()); n != 12 {
		t.Errorf("esecuzioni dopo l'approvazione: %d", n)
	}
}

func TestRolloutFreezePerRegion(t *testing.T) {
	t0 := time.Now()
	gov, err := governance.New(nil, []governance.Window{{Name: "us-maint", Applications: []string{"*"}, Environments: []string{"*"},
		Regions: []string{"us-1"}, From: t0.Add(-time.Minute), To: t0.Add(4 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	env := newWaveEngine(t, "", gov)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-freeze"))
	if err != nil {
		t.Fatal(err)
	}
	st := waitRollout(t, env.e, sub.ID, service.RolloutPausedFreeze)
	if !strings.Contains(st.Rollout.Paused, `freeze "us-maint"`) || !strings.Contains(st.Rollout.Paused, "us-1") || st.Rollout.Stage != "eu-1/eu-1b" {
		t.Fatalf("pausa: %+v", st.Rollout)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) < 4*time.Second {
		t.Error("il rollout non ha atteso la fine del freeze")
	}
}

func TestRolloutBake(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	ctx := context.Background()
	d := waveDescriptor("ondate-bake")
	d.Rollout.Waves = d.Rollout.Waves[:1]
	d.Deploy.Targets = d.Deploy.Targets[:1]
	d.Rollout.Waves[0].Bake = "3s"
	d.Rollout.Health = &descriptor.Health{Interval: "1s", FailuresToStop: 2}
	sub, err := env.e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 3*time.Second || len(env.checker.seen) != 3 {
		t.Errorf("bake di 3s con letture ogni secondo: %s, %d letture", time.Since(start), len(env.checker.seen))
	}
}

// batchDescriptor deploys three nodes of one AZ one at a time, with a gate after every batch.
func batchDescriptor(id string) *descriptor.Descriptor {
	d := waveDescriptor(id)
	d.Deploy.Targets = nil
	for _, h := range []string{"n1", "n2", "n3"} {
		d.Deploy.Targets = append(d.Deploy.Targets, descriptor.Target{Host: h, Labels: map[string]string{"region": "eu-1", "az": "eu-1a"}})
	}
	d.Rollout.Regions = nil
	d.Rollout.Waves = []descriptor.Wave{{Name: "eu-1", Select: "region=eu-1", By: "az", BatchBake: "0s"}}
	d.Rollout.Health = &descriptor.Health{Probes: []descriptor.Probe{{URL: "http://{host}/health"}}}
	return d
}

func TestRolloutGatePerLotto(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, batchDescriptor("lotti-ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	// One read per batch but the last, then the stage gate on the whole pass.
	want := []string{"eu-1/eu-1/eu-1a#0", "eu-1/eu-1/eu-1a#1", "eu-1/eu-1a"}
	if got := env.checker.seen; !slices.Equal(got, want) {
		t.Errorf("letture del gate: %v, attese %v", got, want)
	}
	steps, err := env.e.Steps(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(steps, func(s StepRecord) bool { return s.Name == "gate@eu-1/eu-1/eu-1a#1.1" }) {
		t.Errorf("step del gate per lotto assente: %+v", steps)
	}
}

func TestRolloutGatePerLottoRossoFerma(t *testing.T) {
	env := newWaveEngine(t, "", nil)
	env.checker.redHosts = map[string]bool{"n2": true}
	ctx := context.Background()
	d := batchDescriptor("lotti-rosso")
	d.Rollout.Health.Interval, d.Rollout.Health.FailuresToStop = "1s", 2
	d.Rollout.Waves[0].BatchBake = "2s"
	sub, err := env.e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "after batch eu-1/eu-1/eu-1a#1") {
		t.Fatalf("atteso il gate rosso dopo il secondo lotto, ricevuto %v", err)
	}
	// The third node is never touched.
	want := []string{"deploy:w1:n1", "activation:w1:n1", "deploy:w1:n2", "activation:w1:n2"}
	if got := env.log.get(); !slices.Equal(got, want) {
		t.Errorf("esecuzioni: %v, attese %v", got, want)
	}
}

// waitDeployState waits until the deploy reaches the given state.
func waitDeployState(t *testing.T, e *Engine, id, state string) *Status {
	t.Helper()
	for range 100 {
		if st, err := e.Status(context.Background(), id); err == nil && st.State == state {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("il deploy %s non è arrivato allo stato %s", id, state)
	return nil
}

// waitRollout waits until the rollout reaches the given state.
func waitRollout(t *testing.T, e *Engine, id, state string) *Status {
	t.Helper()
	for range 100 {
		if st, err := e.Status(context.Background(), id); err == nil && st.Rollout != nil && st.Rollout.Status == state {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("il rollout di %s non è arrivato allo stato %s", id, state)
	return nil
}

func TestRolloutInstradatoPerRegion(t *testing.T) {
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	db := testdb.URL(t)
	ch := &fakeChecker{red: map[string]bool{}}
	var e *Engine
	for _, region := range []string{"eu-1", "eu-2", "us-1"} {
		reg, planner := testRegistry(t, waveStep{name: "deploy", worker: region, log: l, slog: sl}, waveStep{name: "activation", worker: region, log: l, slog: sl})
		opts := workerOpts(db, "w-"+region, reg)
		opts.Health, opts.Region, opts.RegionRouting = ch, region, true
		w := startWorker(t, opts)
		if e == nil {
			e = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: ch})
		}
	}
	ctx := context.Background()
	sub, err := e.Submit(ctx, waveDescriptor("ondate-region"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := e.Wait(wctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	calls := l.get()
	if len(calls) != 12 {
		t.Fatalf("esecuzioni: %v", calls)
	}
	for _, c := range calls {
		parts := strings.Split(c, ":") // step:worker:host
		if !strings.HasPrefix(parts[2], parts[1]) {
			t.Errorf("il target %s è stato toccato dal worker di %s", parts[2], parts[1])
		}
	}
	// The logs written by region workers reach the deploy log.
	p, _ := e.Logs(ctx, sub.ID, 0)
	steps := map[string]bool{}
	for _, line := range p.Lines {
		steps[line.Step] = true
	}
	if !steps["deploy@resto/us-1/us-1b#0"] || !steps["activation@canary#0"] {
		t.Errorf("log dei passi mancanti: %v", steps)
	}

	// A region without workers: the pass fails with a clear error.
	d := waveDescriptor("ondate-region-2")
	d.Deploy.Targets = append(d.Deploy.Targets, descriptor.Target{Host: "ap-1a", Labels: map[string]string{"region": "ap-1", "az": "ap-1a"}})
	d.Rollout.Waves[2].Select = "region in (eu-2, us-1, ap-1)"
	sub, err = e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(wctx, sub.ID); err == nil || !strings.Contains(err.Error(), "no worker registered for region ap-1") {
		t.Fatalf("region senza worker: %v", err)
	}
}

// regionChecker simulates a worker that reaches only its own region's targets: probes to
// another region are red, as with separate networks per region.
type regionChecker struct {
	region string
	mu     sync.Mutex
	probed []string
}

func (c *regionChecker) Check(_ context.Context, in rollout.GateInput) rollout.GateResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	res := rollout.GateResult{OK: true}
	for _, p := range in.Passes {
		for range in.Health.Probes {
			for _, t := range p.Targets {
				c.probed = append(c.probed, t.Host)
				ok := p.Region == c.region
				res.OK = res.OK && ok
				res.Checks = append(res.Checks, rollout.CheckResult{Kind: rollout.KindProbe, Host: t.Host, Region: p.Region, OK: ok})
			}
		}
	}
	return res
}

func (c *regionChecker) Supports(*descriptor.Health) error { return nil }

func TestRolloutSondeNellaRegion(t *testing.T) {
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	db := testdb.URL(t)
	checkers := map[string]*regionChecker{}
	var e *Engine
	for _, region := range []string{"eu-1", "eu-2", "us-1"} {
		reg, planner := testRegistry(t, waveStep{name: "deploy", worker: region, log: l, slog: sl}, waveStep{name: "activation", worker: region, log: l, slog: sl})
		checkers[region] = &regionChecker{region: region}
		opts := workerOpts(db, "w-"+region, reg)
		opts.Health, opts.Region, opts.RegionRouting = checkers[region], region, true
		w := startWorker(t, opts)
		if e == nil {
			e = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: checkers[region]})
		}
	}
	d := waveDescriptor("ondate-sonde")
	d.Rollout.Health = &descriptor.Health{Probes: []descriptor.Probe{{URL: "http://{host}/healthz"}}, Interval: "1s"}
	ctx := context.Background()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := e.Wait(wctx, sub.ID); err != nil {
		t.Fatalf("le sonde devono essere eseguite dai worker della region dei target: %v", err)
	}
	for region, c := range checkers {
		c.mu.Lock()
		if len(c.probed) == 0 {
			t.Errorf("il worker di %s non ha eseguito sonde", region)
		}
		for _, h := range c.probed {
			if !strings.HasPrefix(h, region) {
				t.Errorf("il worker di %s ha sondato %s", region, h)
			}
		}
		c.mu.Unlock()
	}
}

func TestRolloutGatePerLottoNellaRegion(t *testing.T) {
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	db := testdb.URL(t)
	checkers := map[string]*regionChecker{}
	var e *Engine
	for _, region := range []string{"eu-1", "us-1"} {
		reg, planner := testRegistry(t, waveStep{name: "deploy", worker: region, log: l, slog: sl}, waveStep{name: "activation", worker: region, log: l, slog: sl})
		checkers[region] = &regionChecker{region: region}
		opts := workerOpts(db, "w-"+region, reg)
		opts.Health, opts.Region, opts.RegionRouting = checkers[region], region, true
		w := startWorker(t, opts)
		if e == nil {
			e = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: checkers[region]})
		}
	}
	ctx := context.Background()
	sub, err := e.Submit(ctx, batchDescriptor("lotti-region"))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := e.Wait(wctx, sub.ID); err != nil {
		t.Fatalf("le sonde dei lotti devono essere eseguite nella region del passo: %v", err)
	}
	// Batch gates on n1 and n2, then the stage gate on the three nodes: all in eu-1.
	if got := checkers["eu-1"].probed; !slices.Equal(got, []string{"n1", "n2", "n1", "n2", "n3"}) {
		t.Errorf("sonde in eu-1: %v", got)
	}
	if got := checkers["us-1"].probed; len(got) != 0 {
		t.Errorf("sonde in us-1: %v", got)
	}
}

func TestRolloutInstradatoConRollback(t *testing.T) {
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	db := testdb.URL(t)
	var e *Engine
	for _, region := range []string{"eu-1", "eu-2", "us-1"} {
		reg, planner := testRegistry(t, waveStep{name: "deploy", worker: region, log: l, fail: "us-1a", slog: sl})
		opts := workerOpts(db, "w-"+region, reg)
		opts.Health, opts.Region, opts.RegionRouting = &fakeChecker{}, region, true
		w := startWorker(t, opts)
		if e == nil {
			e = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: &fakeChecker{}})
		}
	}
	d := waveDescriptor("ondate-region-rb")
	on := true
	d.Rollback = &descriptor.Rollback{Enabled: &on}
	d.Rollout.OnFailure = descriptor.OnFailureRollbackWave
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "rollback completed") {
		t.Fatalf("atteso rollback dello stadio, ricevuto %v", err)
	}
	var undo []string
	for _, c := range l.get() {
		if strings.HasPrefix(c, "undo:") {
			undo = append(undo, strings.TrimPrefix(c, "undo:"))
		}
	}
	// Only the failed stage, every pass compensated by its region's worker.
	slices.Sort(undo)
	if want := []string{"deploy:eu-2:eu-2a", "deploy:us-1:us-1a"}; !slices.Equal(undo, want) {
		t.Fatalf("compensazioni: %v, attese %v", undo, want)
	}
}

// crashOnceStep blocks on its first run on the given host, like a worker dying halfway
// through a pass; from the second it records the run and continues.
type crashOnceStep struct {
	host    string
	log     *waveLog
	calls   *atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (*crashOnceStep) Name() string { return "deploy" }

func (s *crashOnceStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	h := hostsOf(in.Descriptor.Deploy.Targets)
	if h == s.host && s.calls.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	s.log.add(h)
	return pipeline.Output{}, nil
}

func TestRolloutRipresoDopoUnCrash(t *testing.T) {
	// The parallelism stays inside the batch step: with or without it, a recovery resumes from
	// the same DBOS steps.
	for _, parallel := range []string{"", "2"} {
		t.Run("parallel="+parallel, func(t *testing.T) { rolloutCrashRecovery(t, parallel) })
	}
}

func rolloutCrashRecovery(t *testing.T, parallel string) {
	l := &waveLog{}
	step := &crashOnceStep{host: "eu-2a", log: l, calls: new(atomic.Int32), started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(step.release) })
	reg, planner := testRegistry(t, step)
	db := testdb.URL(t)
	opts := workerOpts(db, "w1", reg)
	opts.Health = &fakeChecker{}
	w1, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Launch(); err != nil {
		t.Fatal(err)
	}
	e := NewService(ServiceOptions{Client: w1.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: &fakeChecker{}})
	d := waveDescriptor("ondate-crash")
	d.Deploy.Parallel = parallel
	sub, err := e.Submit(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	<-step.started
	_ = w1.Shutdown(100 * time.Millisecond)

	w := startWorker(t, opts)
	e = NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: &fakeChecker{}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	// Stages already passed are not repeated: canary and eu-1b once only.
	got := l.get()
	if want := []string{"eu-1a", "eu-1b", "eu-2a", "us-1a", "eu-2b", "us-1b"}; !slices.Equal(got, want) {
		t.Fatalf("esecuzioni dopo la ripresa: %v, attese %v", got, want)
	}
}

func TestRolloutApprovazioniSpostateNelRollout(t *testing.T) {
	gov, err := governance.New([]governance.Rule{{Name: "prod", Applications: []string{"*"}, Environments: []string{"*"},
		Approvals: governance.Approvals{Required: 2, Timeout: time.Minute, When: governance.ApprovalsRollout}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := newWaveEngine(t, "", gov)
	ctx := context.Background()
	// Without approval points in the rollout the deploy would start without approvals; nor
	// does a point after the last wave count, since it is never asked.
	if _, err := env.e.Submit(alice, waveDescriptor("ondate-when-senza")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("atteso ErrInvalidRequest, ottenuto %v", err)
	}
	last := waveDescriptor("ondate-when-ultima")
	last.Rollout.ApprovalsBetween = []string{last.Rollout.Waves[len(last.Rollout.Waves)-1].Name}
	if _, err := env.e.Submit(alice, last); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("approvazione dopo l'ultima ondata: atteso ErrInvalidRequest, ottenuto %v", err)
	}
	d := waveDescriptor("ondate-when")
	d.Rollout.ApprovalsBetween = []string{"canary"}
	sub, err := env.e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	// No approval before the deploy: the canary wave starts at once, then the pause asks for the
	// rule's approvals.
	st := waitDeployState(t, env.e, sub.ID, StateAwaitingApproval)
	if st.Rollout == nil || st.Rollout.Approval == nil || st.Rollout.Approval.Required != 2 || len(env.log.get()) != 2 {
		t.Fatalf("pausa inattesa: %+v %v", st.Rollout, env.log.get())
	}
	for _, who := range []context.Context{bob, carol} {
		if err := env.e.Decide(who, sub.ID, DecisionRequest{Approve: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(env.log.get()); n != 12 {
		t.Errorf("esecuzioni dopo l'approvazione: %d", n)
	}
}

// parallelStep records the deploy.parallel every batch (and its compensation) receives.
type parallelStep struct {
	log  *waveLog
	fail string
}

func (parallelStep) Name() string { return "deploy" }

func (s parallelStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	h := hostsOf(in.Descriptor.Deploy.Targets)
	s.log.add(h + "=" + in.Descriptor.Deploy.Parallel)
	if s.fail != "" && strings.Contains(h, s.fail) {
		return nil, pipeline.Permanent(errors.New("broken " + s.fail))
	}
	return pipeline.Output{}, nil
}

func (parallelStep) Checkpoint(context.Context, pipeline.Input) (pipeline.Output, error) {
	return pipeline.Output{}, nil
}

func (s parallelStep) Compensate(_ context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	s.log.add("undo:" + hostsOf(in.Descriptor.Deploy.Targets) + "=" + in.Descriptor.Deploy.Parallel)
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted}, nil
}

func TestRolloutParallelPerWave(t *testing.T) {
	l := &waveLog{}
	reg, planner := testRegistry(t, parallelStep{log: l, fail: "us-1"})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Health = &fakeChecker{red: map[string]bool{}}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Health: opts.Health})

	d := waveDescriptor("parallel-1")
	d.Deploy.Parallel = "50%"
	d.Rollout.MaxUnavailable = "2"
	d.Rollout.OnFailure = descriptor.OnFailureRollbackAll
	d.Rollout.Waves = []descriptor.Wave{
		{Name: "canary", Select: "canary=true", Parallel: "1"},
		{Name: "eu", Select: "region=eu-1, canary!=true"},
		{Name: "rest", Select: "region in (eu-2, us-1)", Parallel: "2"},
	}
	on := true
	d.Rollback = &descriptor.Rollback{Enabled: &on}
	ctx := context.Background()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Fatal("the us-1 batch must fail")
	}
	// Every batch gets its wave's parallel, otherwise deploy.parallel; the compensations get
	// the value their batch ran with.
	want := []string{
		"eu-1a=1", "eu-1b=50%", "eu-2a,eu-2b=2", "us-1a,us-1b=2",
		"undo:us-1a,us-1b=2", "undo:eu-2a,eu-2b=2", "undo:eu-1b=50%", "undo:eu-1a=1",
	}
	if got := l.get(); !slices.Equal(got, want) {
		t.Fatalf("runs:\n %v\nwant\n %v", got, want)
	}
}
