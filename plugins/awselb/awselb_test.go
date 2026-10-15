package awselb

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// fakeAWS emula target group, regole e Auto Scaling group.
type fakeAWS struct {
	mu      sync.Mutex
	health  map[string]string // ID → stato nel target group
	asg     map[string]string // ID → stato nell'Auto Scaling group
	weights map[string]map[string]int32
	calls   []string
}

func (f *fakeAWS) Connect(context.Context, string, *plugin.Request) (API, AutoScaling, error) {
	return f, f, nil
}

func (f *fakeAWS) DescribeTargetHealth(_ context.Context, _ string, ts []Target) ([]TargetHealth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TargetHealth
	for id, st := range f.health {
		if len(ts) > 0 && !slices.ContainsFunc(ts, func(t Target) bool { return t.ID == id }) {
			continue
		}
		out = append(out, TargetHealth{Target: Target{ID: id}, State: st})
	}
	return out, nil
}

func (f *fakeAWS) RegisterTargets(_ context.Context, _ string, ts []Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range ts {
		f.health[t.ID] = "healthy"
		f.calls = append(f.calls, "register "+t.ID)
	}
	return nil
}

func (f *fakeAWS) DeregisterTargets(_ context.Context, _ string, ts []Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range ts {
		f.health[t.ID] = "draining"
		f.calls = append(f.calls, "deregister "+t.ID)
	}
	return nil
}

func (f *fakeAWS) ForwardWeights(_ context.Context, arn string) (map[string]int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.weights[arn], nil
}

func (f *fakeAWS) SetForwardWeights(_ context.Context, arn string, w map[string]int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weights[arn] = w
	return nil
}

func (f *fakeAWS) LifecycleStates(_ context.Context, _ string, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, id := range ids {
		out[id] = f.asg[id]
	}
	return out, nil
}

func (f *fakeAWS) EnterStandby(_ context.Context, _ string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.asg[id], f.health[id] = "Standby", "draining"
		f.calls = append(f.calls, "standby "+id)
	}
	return nil
}

func (f *fakeAWS) ExitStandby(_ context.Context, _ string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.asg[id], f.health[id] = "InService", "healthy"
		f.calls = append(f.calls, "exit "+id)
	}
	return nil
}

const tg = "arn:aws:elasticloadbalancing:eu-west-1:123456789012:targetgroup/shop/0123456789abcdef"

func env(settings map[string]string) traffic.Env {
	targets := []descriptor.Target{{Host: "10.0.0.1", Labels: map[string]string{"instance": "i-1"}}, {Host: "10.0.0.2", Labels: map[string]string{"instance": "i-2"}}}
	return traffic.Env{Settings: settings, Targets: targets, Member: (&descriptor.PoolConfig{Member: "{label.instance}"}).MemberName}
}

func TestPoolTargetGroup(t *testing.T) {
	f := &fakeAWS{health: map[string]string{"i-1": "healthy", "i-2": "healthy"}}
	settings := map[string]string{SettingRegion: "eu-west-1", SettingTargetGroupARN: tg, SettingPort: "8080"}
	if err := ValidatePool(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := NewPool(f)(ctx, env(settings))
	if err != nil {
		t.Fatal(err)
	}
	all, err := p.Members(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("membri: %+v %v", all, err)
	}
	if err := p.Drain(ctx, []string{"i-1"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := p.Connections(ctx, []string{"i-1"}); c["i-1"] != 1 {
		t.Errorf("target in draining senza connessioni: %v", c)
	}
	delete(f.health, "i-1")
	if c, _ := p.Connections(ctx, []string{"i-1"}); c["i-1"] != 0 {
		t.Errorf("target deregistrato con connessioni: %v", c)
	}
	if err := p.Restore(ctx, []string{"i-1"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Healthy(ctx, []string{"i-1"}); !ok["i-1"] {
		t.Error("target rientrato non sano")
	}
	if strings.Join(f.calls, ",") != "deregister i-1,register i-1" {
		t.Errorf("chiamate: %v", f.calls)
	}
}

func TestPoolAutoScaling(t *testing.T) {
	f := &fakeAWS{health: map[string]string{"i-1": "healthy", "i-2": "healthy"}, asg: map[string]string{"i-1": "InService", "i-2": "InService"}}
	settings := map[string]string{SettingRegion: "eu-west-1", SettingTargetGroupARN: tg, SettingAutoScaling: "shop-asg"}
	ctx := context.Background()
	p, err := NewPool(f)(ctx, env(settings))
	if err != nil {
		t.Fatal(err)
	}
	// Ripetuti, svuotamento e rientro non chiamano due volte l'Auto Scaling group.
	for range 2 {
		if err := p.Drain(ctx, []string{"i-1"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := p.Restore(ctx, []string{"i-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(f.calls, ",") != "standby i-1,exit i-1" {
		t.Errorf("chiamate: %v", f.calls)
	}
	f.asg["i-2"] = "Terminating"
	if err := p.Drain(ctx, []string{"i-2"}); err == nil {
		t.Error("istanza in terminazione svuotata")
	}
}

func TestValidatePool(t *testing.T) {
	err := ValidatePool(map[string]string{SettingRegion: "x", SettingTargetGroupARN: "arn:sbagliato", SettingPort: "0"})
	for _, want := range []string{SettingRegion, SettingTargetGroupARN, SettingPort} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("manca l'errore su %s: %v", want, err)
		}
	}
}
