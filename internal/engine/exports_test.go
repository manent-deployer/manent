package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/envconfig"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/testdb"
)

// provisionStep simulates the deploy step: the "infra" application creates VMs and exports
// them (version "*-badexport" produces an invalid list); every deploy records the targets and
// parameters it received.
type provisionStep struct {
	mu   sync.Mutex
	seen map[string]descriptor.Deploy
}

func (*provisionStep) Name() string { return "deploy" }

func (s *provisionStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	d := in.Descriptor
	s.mu.Lock()
	s.seen[in.DeploymentID] = d.Deploy
	s.mu.Unlock()
	out := pipeline.Output{"plugin": "fake"}
	if d.Application == "infra" {
		out[pipeline.OutPluginPrefix+"vms"] = `["10.0.0.1",{"host":"10.0.0.2","port":2222,"labels":{"region":"eu"}}]`
		out[pipeline.OutPluginPrefix+"db"] = "db-" + d.Environment + ".internal"
		if strings.HasSuffix(d.Artifact.Version, "-badexport") {
			out[pipeline.OutPluginPrefix+"vms"] = "non json"
		}
	}
	return out, nil
}

func (s *provisionStep) deployOf(id string) descriptor.Deploy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[id]
}

// envProvider is an environment configuration provider with fixed documents per app/env.
type envProvider map[string]envconfig.EnvConfig

func (p envProvider) Get(_ context.Context, k envconfig.Key) (*envconfig.Document, error) {
	c, ok := p[k.Application+"/"+k.Environment]
	if !ok {
		return nil, envconfig.ErrNotFound
	}
	return &envconfig.Document{Config: c, Source: "test/" + k.Application + "/" + k.Environment}, nil
}

// exportEngine starts a worker with the provisioning step and, if given, the environment
// configuration.
func exportEngine(t *testing.T, env envProvider) (*Engine, *provisionStep) {
	t.Helper()
	step := &provisionStep{seen: map[string]descriptor.Deploy{}}
	reg, planner := testRegistry(t, artifactStep{}, step)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ChainPollInterval = 50 * time.Millisecond
	var resolver ConfigResolver
	if env != nil {
		resolver = envconfig.Resolver{Provider: env, Policy: &envconfig.DescriptorPolicy{Targets: true}}
		opts.Resolver = resolver
	}
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Resolver: resolver, Logger: discard})
	w.SetService(e)
	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(5 * time.Second) })
	return e, step
}

// infraDescriptor creates the VMs and exports their list and the database host.
func infraDescriptor(env, version, id string) *descriptor.Descriptor {
	d := appVersion("infra", env, version, id)
	d.Deploy.Targets = []descriptor.Target{{Host: "vsphere-" + env}}
	d.Exports = map[string]descriptor.Export{
		"vms":     {From: "deploy.vms", Format: descriptor.ExportFormatTargets, Pattern: `10\.0\.0\.\d+`},
		"db_host": {From: "deploy.db", Pattern: `[a-z0-9.-]+`},
	}
	return d
}

// consumerDescriptor configures the VMs exported by the deploy infra.
func consumerDescriptor(env, id, infra string) *descriptor.Descriptor {
	d := appVersion("os-config", env, "1.0.0", id)
	d.After = []string{infra}
	d.Imports = map[string]descriptor.Import{"infra": {Deployment: infra}}
	d.Deploy.TargetsFrom = []descriptor.ExportRef{{Import: "infra", Export: "vms"}}
	d.Deploy.ParamsFrom = map[string]descriptor.ExportRef{"db_host": {Import: "infra", Export: "db_host"}}
	return d
}

func hosts(ts []descriptor.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Host
	}
	return out
}

func TestExportImportTraDeploy(t *testing.T) {
	e, step := exportEngine(t, nil)
	ctx := context.Background()

	if _, err := e.Submit(ctx, infraDescriptor("test", "1.0.0", "infra-1")); err != nil {
		t.Fatal(err)
	}
	// The consumer is submitted while the producer may still be running: the import is
	// resolved when it starts.
	c := consumerDescriptor("test", "os-1", "infra-1")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(ctx, c); err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(ctx, "os-1")
	if err != nil {
		t.Fatalf("consumatore: %v", err)
	}
	got := step.deployOf("os-1")
	if !slices.Equal(hosts(got.Targets), []string{"10.0.0.1", "10.0.0.2"}) || got.Targets[1].Port != 2222 || got.Targets[1].Labels["region"] != "eu" {
		t.Errorf("target importati: %+v", got.Targets)
	}
	if got.Params["db_host"] != "db-test.internal" {
		t.Errorf("parametri importati: %+v", got.Params)
	}
	if res.Steps[0].Name != "config" || res.Steps[0].Output["import.infra"] != "infra-1@1.0.0" || res.Steps[0].Output["targets"] != "2" {
		t.Errorf("meta dello step config: %+v", res.Steps[0])
	}
	st, err := e.Status(ctx, "infra-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Exports["db_host"] != "db-test.internal" || !strings.Contains(st.Exports["vms"], "10.0.0.2") {
		t.Errorf("export del produttore: %+v", st.Exports)
	}

	// An export that does not match its format fails the producer; the consumer does not run.
	if _, err := e.Submit(ctx, infraDescriptor("qa", "2.0.0-badexport", "infra-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(ctx, consumerDescriptor("qa", "os-2", "infra-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, "infra-2"); err == nil || !strings.Contains(err.Error(), "exports.vms") {
		t.Errorf("export non valido: %v", err)
	}
	waitState(t, e, "os-2", StateDependencyFailed, nil)

	// An export the producer does not declare is a permanent error of the consumer.
	missing := consumerDescriptor("test", "os-3", "infra-1")
	missing.Deploy.ParamsFrom = map[string]descriptor.ExportRef{"db_host": {Import: "infra", Export: "nope"}}
	if _, err := e.Submit(ctx, missing); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), `no export "nope"`) {
		t.Errorf("export inesistente all'accodamento: %v", err)
	}
}

func TestExportImportNellaRelease(t *testing.T) {
	e, step := exportEngine(t, nil)
	ctx := context.Background()
	c := consumerDescriptor("prod", "", "infra")
	c.After = nil // the dependency is the release's
	r := &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: "r1", Deploys: []descriptor.ReleaseDeploy{
		{Name: "infra", Descriptor: infraDescriptor("prod", "1.0.0", "")},
		{Name: "os", Descriptor: c, After: []string{"infra"}},
	}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SubmitRelease(ctx, r); err != nil {
		t.Fatal(err)
	}
	// The release enqueues its deploys in its first step.
	waitState(t, e, "r1.os", StateSuccess, nil)
	if got := step.deployOf("r1.os"); !slices.Equal(hosts(got.Targets), []string{"10.0.0.1", "10.0.0.2"}) || got.Params["db_host"] != "db-prod.internal" {
		t.Errorf("import nella release: %+v", got)
	}

	// An import of a release deploy that is not among the dependencies is rejected.
	bad := *r
	bad.ID = "r2"
	bad.Deploys = slices.Clone(r.Deploys)
	bad.Deploys[1].After = nil
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "must be among the dependencies") {
		t.Errorf("import senza dipendenza: %v", err)
	}
}

func TestImportDallaReleaseInEsercizio(t *testing.T) {
	ref := map[string]descriptor.Import{"infra": {Current: &descriptor.ImportCurrent{Application: "infra"}}}
	e, step := exportEngine(t, envProvider{
		"os-config/test": {Imports: ref, TargetsFrom: []descriptor.ExportRef{{Import: "infra", Export: "vms"}},
			ParamsFrom: map[string]descriptor.ExportRef{"db_host": {Import: "infra", Export: "db_host"}}},
		"os-config/prod": {Imports: ref, TargetsFrom: []descriptor.ExportRef{{Import: "infra", Export: "vms"}}},
	})
	ctx := context.Background()

	// Without a release in service of infra the deploy is rejected at once.
	if _, err := e.Submit(ctx, appVersion("os-config", "test", "1.0.0", "os-0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "no release in service of infra/test") {
		t.Fatalf("senza release in esercizio: %v", err)
	}
	infra := infraDescriptor("test", "1.0.0", "infra-1")
	deploy(t, e, ctx, infra)
	deploy(t, e, ctx, appVersion("os-config", "test", "1.0.0", "os-1"))
	if got := step.deployOf("os-1"); !slices.Equal(hosts(got.Targets), []string{"10.0.0.1", "10.0.0.2"}) || got.Params["db_host"] != "db-test.internal" {
		t.Errorf("import dalla configurazione di ambiente: %+v", got)
	}

	// The environment configuration defines the targets: the descriptor cannot add its own.
	own := appVersion("os-config", "test", "1.0.1", "os-2")
	own.Deploy.Targets = []descriptor.Target{{Host: "altro"}}
	if _, err := e.Submit(ctx, own); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "targets_from") {
		t.Errorf("target del descrittore: %v", err)
	}

	// A promotion reads the exports of the target environment.
	deploy(t, e, ctx, infraDescriptor("prod", "1.0.0", "infra-p"))
	sub, err := e.Promote(ctx, PromoteRequest{Application: "os-config", From: "test", To: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := step.deployOf(sub.ID); len(got.Targets) != 2 || got.Params["db_host"] != "" {
		t.Errorf("promozione: %+v", got)
	}
}
