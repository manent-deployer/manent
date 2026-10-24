// Command fakeplugin is an external plugin used by the extplugin tests.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/pkg/plugin/external"
)

type fake struct {
	log *slog.Logger
}

func (fake) Name() string           { return "fake" }
func (fake) Supports(t string) bool { return t == "application" }
func (fake) Validate(r *plugin.Request) error {
	if r.Params["dest"] == "" {
		return errors.New("dest parameter is required")
	}
	return nil
}

// Deploy reads the given secret and reports what it sees of its environment.
func (f fake) Deploy(ctx context.Context, r *plugin.Request) (*plugin.Result, error) {
	if r.Params["crash"] == "true" {
		os.Exit(3)
	}
	if r.Params["permanent"] == "true" {
		return nil, plugin.Permanent(errors.New("refused by a safety limit"))
	}
	secret, err := r.Secrets.Secret(ctx, r.Params["secret"])
	if err != nil {
		return nil, err
	}
	f.log.Info("fake deploy done", "deployment", r.DeploymentID)
	return &plugin.Result{Plugin: "fake", Outputs: map[string]string{"vms": `["10.0.0.1"]`}, Targets: []plugin.TargetResult{{
		Host:   r.Targets[0].Host,
		Status: plugin.StatusDeployed,
		Details: map[string]string{
			"secret_len": strconv.Itoa(len(secret)),
			"leak":       os.Getenv("LEAK_ME"),
			"foo":        os.Getenv("FOO"),
			"pid":        strconv.Itoa(os.Getpid()),
			"region":     r.Targets[0].Labels["region"],
		},
	}}}, nil
}

// dryFake also supports dry-run.
type dryFake struct{ fake }

func (dryFake) DryRun(_ context.Context, r *plugin.Request) (*plugin.Result, error) {
	return &plugin.Result{Plugin: "fake", Targets: []plugin.TargetResult{{Host: r.Targets[0].Host, Status: plugin.StatusReady}}}, nil
}

// rbFake supports reading the current release and rollback.
type rbFake struct{ fake }

func (rbFake) CurrentRelease(_ context.Context, r *plugin.Request) (*plugin.Release, error) {
	if r.Params["first"] == "true" {
		return nil, nil
	}
	return &plugin.Release{Version: "1.0.0", Details: map[string]string{"source": "fake"}}, nil
}

// Rollback reads the secret, like a deploy, and reports the restored release.
func (rbFake) Rollback(ctx context.Context, r *plugin.Request, prev plugin.Release) (*plugin.Result, error) {
	secret, err := r.Secrets.Secret(ctx, r.Params["secret"])
	if err != nil {
		return nil, err
	}
	return &plugin.Result{Plugin: "fake", Targets: []plugin.TargetResult{{
		Host:    r.Targets[0].Host,
		Status:  plugin.StatusRolledBack,
		Details: map[string]string{"release": prev.Version, "secret_len": strconv.Itoa(len(secret))},
	}}}, nil
}

// slotFake supports percentage rollouts: the recorded state comes back in every call.
type slotFake struct {
	fake
	weight *int
}

func (slotFake) PrepareSlots(context.Context, *plugin.Request) (map[string]string, error) {
	return map[string]string{plugin.StateHasStable: "true", "stable": "a"}, nil
}

func (slotFake) DeployCandidate(ctx context.Context, r *plugin.Request) (*plugin.Result, error) {
	secret, err := r.Secrets.Secret(ctx, r.Params["secret"])
	if err != nil {
		return nil, err
	}
	return &plugin.Result{Plugin: "fake", Targets: []plugin.TargetResult{{Host: r.Targets[0].Host, Status: plugin.StatusDeployed,
		Details: map[string]string{"stable": r.Traffic.State["stable"], "header": r.Traffic.TesterHeader, "secret_len": strconv.Itoa(len(secret))}}}}, nil
}

func (slotFake) PromoteCandidate(context.Context, *plugin.Request) error { return nil }
func (slotFake) RevertCandidate(context.Context, *plugin.Request) error  { return nil }
func (slotFake) RetirePrevious(context.Context, *plugin.Request) error   { return nil }

func (s slotFake) SetCandidateWeight(_ context.Context, _ *plugin.Request, p int) error {
	*s.weight = p
	return nil
}

func (s slotFake) CandidateWeight(context.Context, *plugin.Request) (int, error) {
	return *s.weight, nil
}

func factory(cfg map[string]any, log *slog.Logger) (plugin.Deployer, error) {
	if v, _ := cfg["slots"].(bool); v {
		return slotFake{fake: fake{log: log}, weight: new(int)}, nil
	}
	if v, _ := cfg["fail"].(bool); v {
		return nil, errors.New("configuration rejected")
	}
	if v, _ := cfg["rollback"].(bool); v {
		return rbFake{fake{log: log}}, nil
	}
	if v, _ := cfg["dry_run"].(bool); v {
		return dryFake{fake{log: log}}, nil
	}
	return fake{log: log}, nil
}

func main() {
	external.Serve(factory)
}
