package steps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/internal/artifact"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/secrets"
	"github.com/manent-deployer/manent/pkg/plugin"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeDeployer is a minimal plugin for the selection and deploy tests.
type fakeDeployer struct {
	name  string
	types []string
	got   *plugin.Request
	err   error
}

func (f *fakeDeployer) Name() string { return f.name }
func (f *fakeDeployer) Supports(t string) bool {
	for _, x := range f.types {
		if x == t {
			return true
		}
	}
	return false
}
func (f *fakeDeployer) Validate(*plugin.Request) error { return nil }
func (f *fakeDeployer) Deploy(_ context.Context, r *plugin.Request) (*plugin.Result, error) {
	f.got = r
	if f.err != nil {
		return nil, f.err
	}
	return &plugin.Result{Plugin: f.name}, nil
}

func newPlugins(t *testing.T, deps ...*fakeDeployer) *plugin.Registry {
	t.Helper()
	reg := plugin.NewRegistry()
	for _, d := range deps {
		d := d
		if err := reg.Register(d.name, func(map[string]any, *slog.Logger) (plugin.Deployer, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
		if err := reg.Activate(d.name, nil, discard); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func TestDefaultPolicy(t *testing.T) {
	reg := newPlugins(t,
		&fakeDeployer{name: "b-infra", types: []string{"infrastructure"}},
		&fakeDeployer{name: "a-app", types: []string{"application"}},
	)
	pol := DefaultPolicy{Plugins: reg}

	d := &descriptor.Descriptor{Artifact: descriptor.Artifact{Type: "infrastructure"}}
	if dep, err := pol.Select(d); err != nil || dep.Name() != "b-infra" {
		t.Fatalf("selezione per tipo errata: %v %v", dep, err)
	}
	d.Deploy.Plugin = "a-app"
	if _, err := pol.Select(d); err == nil {
		t.Fatal("atteso errore: plugin esplicito non supporta il tipo")
	}
	d.Artifact.Type = "sconosciuto"
	d.Deploy.Plugin = ""
	if _, err := pol.Select(d); err == nil {
		t.Fatal("atteso errore: nessun plugin per il tipo")
	}
}

func TestDeployPassaArtefattoAlPlugin(t *testing.T) {
	dep := &fakeDeployer{name: "app", types: []string{"application"}}
	store := &fakeStore{path: "/altro-worker/app.tgz"}
	s := Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: newPlugins(t, dep)}}, Artifacts: store, Logger: discard}
	in := pipeline.Input{
		DeploymentID: "d1",
		Descriptor: &descriptor.Descriptor{
			Artifact: descriptor.Artifact{Type: "application"},
			Deploy:   descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}},
		},
		Outputs: map[string]pipeline.Output{"artifacts": {OutLocalPath: "/w/app.tgz", OutSHA256: "abc"}},
	}
	out, err := s.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// The plugin receives the path guaranteed by the store, not the one recorded by the artifacts step.
	if dep.got.Artifact.LocalPath != "/altro-worker/app.tgz" || dep.got.Targets[0].Host != "h1" || out["plugin"] != "app" {
		t.Errorf("richiesta al plugin inattesa: %+v", dep.got)
	}
}

func TestDeployErrorePermanenteDelPlugin(t *testing.T) {
	for _, tc := range []struct {
		err       error
		permanent bool
	}{{plugin.Permanent(errors.New("over max_destroy")), true}, {errors.New("connection reset"), false}} {
		dep := &fakeDeployer{name: "infra", types: []string{"terraform"}, err: tc.err}
		s := Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: newPlugins(t, dep)}}, Artifacts: &fakeStore{path: "/w/m.tgz"}, Logger: discard}
		in := pipeline.Input{
			DeploymentID: "d1",
			Descriptor: &descriptor.Descriptor{
				Artifact: descriptor.Artifact{Type: "terraform"},
				Deploy:   descriptor.Deploy{Targets: []descriptor.Target{{Host: "prod-eu"}}},
			},
			Outputs: map[string]pipeline.Output{"artifacts": {OutLocalPath: "/w/m.tgz", OutSHA256: "abc"}},
		}
		_, err := s.Run(context.Background(), in)
		if err == nil || errors.Is(err, pipeline.ErrPermanent) != tc.permanent {
			t.Errorf("%v: permanente=%v atteso %v", err, errors.Is(err, pipeline.ErrPermanent), tc.permanent)
		}
	}
}

// dryDeployer supports dry-run and records whether the real deploy ran.
type dryDeployer struct {
	fakeDeployer
	dry bool
}

func (f *dryDeployer) DryRun(_ context.Context, r *plugin.Request) (*plugin.Result, error) {
	f.dry = true
	return &plugin.Result{Plugin: f.name, Targets: []plugin.TargetResult{{Host: r.Targets[0].Host, Status: plugin.StatusReady}}}, nil
}

func TestDeployDryRun(t *testing.T) {
	in := pipeline.Input{
		DeploymentID: "d1",
		Descriptor: &descriptor.Descriptor{
			Artifact: descriptor.Artifact{Type: "application"},
			Deploy:   descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}},
		},
		Outputs: map[string]pipeline.Output{"artifacts": {OutLocalPath: "/w/app.tgz", OutSHA256: "abc"}},
	}

	// Plugin with DryRun: it is called instead of Deploy.
	dep := &dryDeployer{fakeDeployer: fakeDeployer{name: "app", types: []string{"application"}}}
	reg := plugin.NewRegistry()
	_ = reg.Register(dep.name, func(map[string]any, *slog.Logger) (plugin.Deployer, error) { return dep, nil })
	_ = reg.Activate(dep.name, nil, discard)
	s := Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: reg}}, Artifacts: &fakeStore{path: "/w/app.tgz"}, Logger: discard}
	out, err := s.DryRun(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !dep.dry || dep.got != nil || out["dry_run"] != "checked" || out["result"] == "" {
		t.Fatalf("dry-run inatteso: dry=%v deploy=%v out=%v", dep.dry, dep.got != nil, out)
	}

	// Plugin without DryRun: validation only, no deploy.
	plain := &fakeDeployer{name: "app", types: []string{"application"}}
	s.Policies[""] = DefaultPolicy{Plugins: newPlugins(t, plain)}
	if out, err = s.DryRun(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if plain.got != nil || out["dry_run"] != "validated" {
		t.Fatalf("dry-run inatteso: deploy=%v out=%v", plain.got != nil, out)
	}
}

// fakeStore simulates a store that downloads the artifact again to another path.
type fakeStore struct {
	path string
}

func (f *fakeStore) Fetch(context.Context, string, string) (*artifact.Fetched, error) {
	return nil, nil
}

func (f *fakeStore) Ensure(_ context.Context, _, _, localPath, _ string) (string, bool, error) {
	return f.path, localPath != f.path, nil
}

func TestQAChecksum(t *testing.T) {
	q := QA{Checkers: []Checker{NonEmptyChecker{}, ChecksumChecker{}}, Logger: discard}
	in := pipeline.Input{
		Descriptor: &descriptor.Descriptor{Artifact: descriptor.Artifact{SHA256: "ABC"}},
		Outputs:    map[string]pipeline.Output{"artifacts": {OutSHA256: "abc", OutSize: "10"}},
	}
	if _, err := q.Run(context.Background(), in); err != nil {
		t.Fatalf("QA doveva passare: %v", err)
	}
	in.Outputs["artifacts"][OutSHA256] = "def"
	if _, err := q.Run(context.Background(), in); err == nil {
		t.Fatal("QA doveva fallire per checksum diverso")
	}
}

func TestSecretsCallbackNonDichiarato(t *testing.T) {
	s := Secrets{Provider: secrets.NewDefaultProvider(), Logger: discard}
	in := pipeline.Input{Descriptor: &descriptor.Descriptor{
		Callbacks: []descriptor.Callback{{URL: "https://ci.example.com", Auth: &descriptor.CallbackAuth{Type: "bearer", Secret: "ci-token"}}},
	}}
	if _, err := s.Run(context.Background(), in); err == nil {
		t.Fatal("atteso errore: segreto del callback non dichiarato")
	}
}

// rollbackDeployer reads the current release and records the rollback.
type rollbackDeployer struct {
	fakeDeployer
	current  *plugin.Release
	restored string
}

func (f *rollbackDeployer) CurrentRelease(context.Context, *plugin.Request) (*plugin.Release, error) {
	return f.current, nil
}

func (f *rollbackDeployer) Rollback(_ context.Context, _ *plugin.Request, prev plugin.Release) (*plugin.Result, error) {
	f.restored = prev.Version
	return &plugin.Result{Plugin: f.name}, nil
}

func TestDeployCheckpointECompensate(t *testing.T) {
	on := true
	in := pipeline.Input{
		DeploymentID: "d1",
		Descriptor: &descriptor.Descriptor{
			Artifact: descriptor.Artifact{Type: "application"},
			Deploy:   descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}},
			Rollback: &descriptor.Rollback{Enabled: &on, Locator: plugin.PluginLocatorName},
		},
		Outputs: map[string]pipeline.Output{"artifacts": {OutLocalPath: "/w/app.tgz"}},
	}
	dep := &rollbackDeployer{fakeDeployer: fakeDeployer{name: "app", types: []string{"application"}}, current: &plugin.Release{Version: "1.0.0"}}
	reg := plugin.NewRegistry()
	_ = reg.Register(dep.name, func(map[string]any, *slog.Logger) (plugin.Deployer, error) { return dep, nil })
	_ = reg.Activate(dep.name, nil, discard)
	s := Deploy{
		Policies: map[string]Policy{"": DefaultPolicy{Plugins: reg}},
		Locators: map[string]plugin.ReleaseLocator{plugin.PluginLocatorName: plugin.PluginLocator{}},
		Logger:   discard,
	}
	cp, err := s.Checkpoint(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if cp[OutPreviousVersion] != "1.0.0" || cp[OutLocator] != "plugin" {
		t.Fatalf("checkpoint inatteso: %v", cp)
	}
	out, err := s.Compensate(context.Background(), in, cp)
	if err != nil {
		t.Fatal(err)
	}
	if dep.restored != "1.0.0" || out[pipeline.OutRollback] != pipeline.RollbackCompleted {
		t.Fatalf("rollback inatteso: restored=%q out=%v", dep.restored, out)
	}

	// First deploy: no previous release, the compensation does not call the plugin.
	dep.current, dep.restored = nil, ""
	if cp, err = s.Checkpoint(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if out, err = s.Compensate(context.Background(), in, cp); err != nil {
		t.Fatal(err)
	}
	if dep.restored != "" || out[pipeline.OutRollback] != pipeline.RollbackSkipped {
		t.Fatalf("compensazione inattesa: %v", out)
	}

	// Plugin without Rollbacker: the checkpoint fails before the deploy.
	s.Policies[""] = DefaultPolicy{Plugins: newPlugins(t, &fakeDeployer{name: "app", types: []string{"application"}})}
	if _, err := s.Checkpoint(context.Background(), in); err == nil || !strings.Contains(err.Error(), "does not support it") {
		t.Fatalf("atteso errore di rollback non supportato, ricevuto %v", err)
	}
}

func TestActivationCompensate(t *testing.T) {
	a := Activation{Activator: LogActivator{Logger: discard}}
	in := pipeline.Input{DeploymentID: "d1", Descriptor: &descriptor.Descriptor{}}
	out, err := a.Compensate(context.Background(), in, nil)
	if err != nil || out[pipeline.OutRollback] != pipeline.RollbackCompleted {
		t.Fatalf("compensazione inattesa: %v %v", out, err)
	}
}

func TestDeployOutputDelPlugin(t *testing.T) {
	out, err := deployOutput("infra", &plugin.Result{Plugin: "infra", Outputs: map[string]string{"vms": `["10.0.0.1"]`, "db.host": "db1"}})
	if err != nil {
		t.Fatal(err)
	}
	if out["out.vms"] != `["10.0.0.1"]` || out["out.db.host"] != "db1" || strings.Contains(out["result"], "outputs") {
		t.Errorf("output dello step: %+v", out)
	}
	for _, bad := range []map[string]string{{"": "x"}, {"a b": "x"}, {"k": strings.Repeat("x", plugin.MaxOutputBytes+1)}} {
		if _, err := deployOutput("infra", &plugin.Result{Outputs: bad}); err == nil || !strings.Contains(err.Error(), "invalid output") {
			t.Errorf("output non valido %v: %v", bad, err)
		}
	}
}
