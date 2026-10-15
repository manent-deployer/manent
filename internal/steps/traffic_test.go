package steps

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// slotDeployer è un plugin che rilascia versioni affiancate e sposta il traffico.
type slotDeployer struct {
	fakeDeployer
	calls  []string
	weight int
}

func (s *slotDeployer) PrepareSlots(context.Context, *plugin.Request) (map[string]string, error) {
	return map[string]string{plugin.StateHasStable: "true", "stable": "a", "candidate": "b", "stable@h1": "a"}, nil
}

func (s *slotDeployer) DeployCandidate(_ context.Context, r *plugin.Request) (*plugin.Result, error) {
	s.calls = append(s.calls, "candidate:"+r.Traffic.State["stable@h1"]+":"+r.Traffic.TesterHeader)
	return &plugin.Result{Plugin: s.name}, nil
}

func (s *slotDeployer) PromoteCandidate(context.Context, *plugin.Request) error {
	s.calls = append(s.calls, "promote")
	return nil
}

func (s *slotDeployer) RevertCandidate(context.Context, *plugin.Request) error {
	s.calls = append(s.calls, "revert")
	return nil
}

func (s *slotDeployer) RetirePrevious(context.Context, *plugin.Request) error {
	s.calls = append(s.calls, "retire")
	return nil
}

func (s *slotDeployer) SetCandidateWeight(_ context.Context, _ *plugin.Request, p int) error {
	s.weight = p
	s.calls = append(s.calls, "weight:"+strconv.Itoa(p))
	return nil
}

func (s *slotDeployer) CandidateWeight(context.Context, *plugin.Request) (int, error) {
	return s.weight, nil
}

func slotRegistry(t *testing.T, d plugin.Deployer) *plugin.Registry {
	t.Helper()
	reg := plugin.NewRegistry()
	if err := reg.Register(d.Name(), func(map[string]any, *slog.Logger) (plugin.Deployer, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	if err := reg.Activate(d.Name(), nil, discard); err != nil {
		t.Fatal(err)
	}
	return reg
}

func trafficInput(d *descriptor.Descriptor) pipeline.Input {
	return pipeline.Input{DeploymentID: "d1", Descriptor: d, Outputs: map[string]pipeline.Output{"artifacts": {OutLocalPath: "/w/app", OutSHA256: "abc"}}}
}

func TestTrafficConIlPlugin(t *testing.T) {
	dep := &slotDeployer{fakeDeployer: fakeDeployer{name: "slots", types: []string{"application"}}}
	deploy := Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: slotRegistry(t, dep)}}, Artifacts: &fakeStore{path: "/w/app"}, Logger: discard}
	tr := Traffic{Deploy: deploy, Providers: traffic.Default(), Logger: discard}
	d := &descriptor.Descriptor{
		Artifact: descriptor.Artifact{Type: "application", Version: "2.0.0"},
		Deploy:   descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}},
		Rollout: &descriptor.Rollout{Strategy: descriptor.StrategyTraffic, Traffic: &descriptor.TrafficRollout{
			Steps: []int{100}, Testers: &descriptor.Testers{Header: "x-canary: 1"}}},
	}
	if err := tr.Check(d); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := trafficInput(d)
	prep, err := tr.Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if prep[OutMode] != ModePlugin || prep[OutHasStable] != "true" || prep[OutCandidate] != "b" {
		t.Fatalf("preparazione: %v", prep)
	}
	in.Outputs[TrafficOutput] = prep
	// Lo step deploy, con lo stato del rilascio negli output, rilascia la candidata.
	if _, err := deploy.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	if dep.got != nil {
		t.Error("deploy in place eseguito invece della candidata")
	}
	// Il checkpoint non legge la release: il ritorno è uno spostamento di traffico.
	if cp, err := deploy.Checkpoint(ctx, in); err != nil || cp["mode"] != "traffic" {
		t.Errorf("checkpoint: %v %v", cp, err)
	}
	if _, err := tr.SetWeight(ctx, in, 25); err != nil {
		t.Fatal(err)
	}
	if w, _ := tr.Weight(ctx, in); w["weight"] != "25" {
		t.Errorf("peso: %v", w)
	}
	for _, fn := range []func(context.Context, pipeline.Input) (pipeline.Output, error){tr.Promote, tr.Revert, tr.Retire} {
		if _, err := fn(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if want := "candidate:a:x-canary: 1,weight:25,promote,revert,retire"; strings.Join(dep.calls, ",") != want {
		t.Errorf("chiamate: %v", dep.calls)
	}
	// I tester richiedono un plugin che sappia instradarli.
	if _, err := tr.Testers(ctx, in); err == nil || !strings.Contains(err.Error(), "non sa instradare i tester") {
		t.Errorf("tester: %v", err)
	}
	// Un plugin senza versioni affiancate richiede un router dell'ambiente.
	plain := Traffic{Deploy: Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: newPlugins(t, &fakeDeployer{name: "plain", types: []string{"application"}})}}, Logger: discard}, Providers: traffic.Default()}
	if err := plain.Check(d); err == nil || !strings.Contains(err.Error(), "traffic.router") {
		t.Errorf("plugin senza slot: %v", err)
	}
}

// fakeRouter divide il traffico tra gli slot a e b.
type fakeRouter struct{ w map[string]int }

func (r *fakeRouter) Weights(context.Context) (map[string]int, error) { return r.w, nil }

func (r *fakeRouter) SetWeights(_ context.Context, w map[string]int) error {
	for k, v := range w {
		r.w[k] = v
	}
	return nil
}

func TestTrafficConIlRouter(t *testing.T) {
	r := &fakeRouter{w: map[string]int{"a": 100, "b": 0}}
	providers := traffic.Default()
	providers.Routers["fake"] = traffic.Provider[traffic.RouterFactory]{New: func(context.Context, traffic.Env) (traffic.Router, error) { return r, nil }}
	tr := Traffic{Providers: providers, Logger: discard}
	d := &descriptor.Descriptor{
		Deploy: descriptor.Deploy{Targets: []descriptor.Target{
			{Host: "vm1", Labels: map[string]string{"slot": "a"}}, {Host: "vm2", Labels: map[string]string{"slot": "b"}}, {Host: "vm3", Labels: map[string]string{"slot": "b"}},
		}},
		Rollout: &descriptor.Rollout{Strategy: descriptor.StrategyTraffic, Traffic: &descriptor.TrafficRollout{Steps: []int{50, 100}}},
		Traffic: &descriptor.TrafficConfig{Router: &descriptor.RouterConfig{Provider: "fake"}},
	}
	if err := tr.Check(d); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := trafficInput(d)
	prep, err := tr.Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if prep[OutStable] != "a" || prep[OutCandidate] != "b" || prep[OutCandidates] != "vm2,vm3" {
		t.Fatalf("preparazione: %v", prep)
	}
	in.Outputs[TrafficOutput] = prep
	if ts := CandidateTargets(d, prep); len(ts) != 2 || ts[0].Host != "vm2" {
		t.Errorf("target candidati: %+v", ts)
	}
	if _, err := tr.SetWeight(ctx, in, 50); err != nil || r.w["a"] != 50 || r.w["b"] != 50 {
		t.Fatalf("pesi: %v %v", r.w, err)
	}
	if out, _ := tr.Revert(ctx, in); out[pipeline.OutRollback] != pipeline.RollbackCompleted || r.w["a"] != 100 || r.w["b"] != 0 {
		t.Errorf("ritorno: %v %v", out, r.w)
	}
	if _, err := tr.Promote(ctx, in); err != nil || r.w["b"] != 100 || r.w["a"] != 0 {
		t.Errorf("promozione: %v %v", r.w, err)
	}
	// Ogni target deve dichiarare lo slot.
	d.Deploy.Targets = append(d.Deploy.Targets, descriptor.Target{Host: "vm4"})
	if err := tr.Check(d); err == nil || !strings.Contains(err.Error(), "vm4") {
		t.Errorf("slot mancante: %v", err)
	}
	if !slices.Contains(descriptor.Slots, descriptor.OtherSlot("")) {
		t.Error("slot di default")
	}
}
