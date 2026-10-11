// Command fakeplugin è un plugin esterno usato dai test di extplugin.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/pkg/plugin/external"
)

type fake struct {
	log *slog.Logger
}

func (fake) Name() string           { return "fake" }
func (fake) Supports(t string) bool { return t == "application" }
func (fake) Validate(r *plugin.Request) error {
	if r.Params["dest"] == "" {
		return errors.New("parametro dest obbligatorio")
	}
	return nil
}

// Deploy legge il segreto indicato e riporta ciò che vede del proprio ambiente.
func (f fake) Deploy(ctx context.Context, r *plugin.Request) (*plugin.Result, error) {
	if r.Params["crash"] == "true" {
		os.Exit(3)
	}
	secret, err := r.Secrets.Secret(ctx, r.Params["secret"])
	if err != nil {
		return nil, err
	}
	f.log.Info("deploy finto eseguito", "deployment", r.DeploymentID)
	return &plugin.Result{Plugin: "fake", Targets: []plugin.TargetResult{{
		Host:   r.Targets[0].Host,
		Status: plugin.StatusDeployed,
		Details: map[string]string{
			"secret_len": strconv.Itoa(len(secret)),
			"leak":       os.Getenv("LEAK_ME"),
			"foo":        os.Getenv("FOO"),
			"pid":        strconv.Itoa(os.Getpid()),
		},
	}}}, nil
}

// dryFake supporta anche il dry-run.
type dryFake struct{ fake }

func (dryFake) DryRun(_ context.Context, r *plugin.Request) (*plugin.Result, error) {
	return &plugin.Result{Plugin: "fake", Targets: []plugin.TargetResult{{Host: r.Targets[0].Host, Status: plugin.StatusReady}}}, nil
}

// rbFake supporta la lettura della release corrente e il rollback.
type rbFake struct{ fake }

func (rbFake) CurrentRelease(_ context.Context, r *plugin.Request) (*plugin.Release, error) {
	if r.Params["first"] == "true" {
		return nil, nil
	}
	return &plugin.Release{Version: "1.0.0", Details: map[string]string{"source": "fake"}}, nil
}

// Rollback legge il segreto, come un deploy, e riporta la release ripristinata.
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

func factory(cfg map[string]any, log *slog.Logger) (plugin.Deployer, error) {
	if v, _ := cfg["fail"].(bool); v {
		return nil, errors.New("configurazione rifiutata")
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
