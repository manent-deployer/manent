package steps

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/joeg-ita/manent/internal/artifact"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/pkg/plugin"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeDeployer è un plugin minimale per i test di selezione e deploy.
type fakeDeployer struct {
	name  string
	types []string
	got   *plugin.Request
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
	// Il plugin riceve il percorso garantito dallo store, non quello registrato dallo step artifacts.
	if dep.got.Artifact.LocalPath != "/altro-worker/app.tgz" || dep.got.Targets[0].Host != "h1" || out["plugin"] != "app" {
		t.Errorf("richiesta al plugin inattesa: %+v", dep.got)
	}
}

// fakeStore simula uno store che riscarica l'artefatto su un altro percorso.
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
