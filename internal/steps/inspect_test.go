package steps

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// perHostDeployer reads a different release on every target.
type perHostDeployer struct {
	fakeDeployer
	versions map[string]string
}

func (f *perHostDeployer) CurrentRelease(_ context.Context, r *plugin.Request) (*plugin.Release, error) {
	if len(r.Targets) != 1 {
		return nil, errors.New("atteso un target per richiesta")
	}
	switch v := f.versions[r.Targets[0].Host]; v {
	case "":
		return nil, nil
	case "errore":
		return nil, errors.New("ssh: connessione rifiutata")
	default:
		return &plugin.Release{Version: v}, nil
	}
}

func TestInspectTargets(t *testing.T) {
	dep := &perHostDeployer{fakeDeployer: fakeDeployer{name: "app", types: []string{"application"}}, versions: map[string]string{"a": "2.0.0", "c": "errore"}}
	reg := plugin.NewRegistry()
	_ = reg.Register(dep.name, func(map[string]any, *slog.Logger) (plugin.Deployer, error) { return dep, nil })
	_ = reg.Activate(dep.name, nil, discard)
	s := Deploy{Policies: map[string]Policy{"": DefaultPolicy{Plugins: reg}}, Logger: discard}
	d := &descriptor.Descriptor{Application: "shop", Environment: "prod", Artifact: descriptor.Artifact{Type: "application"}}
	ts := []descriptor.Target{{Host: "a"}, {Host: "b"}, {Host: "c"}}
	vs, errs, err := s.InspectTargets(context.Background(), d, ts, time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if vs[0] != "2.0.0" || vs[1] != "" || errs[1] != nil || errs[2] == nil {
		t.Errorf("letture: %q %v", vs, errs)
	}

	// A plugin that cannot read the release in service: the whole environment is unreadable.
	plain := newPlugins(t, &fakeDeployer{name: "plain", types: []string{"application"}})
	s.Policies[""] = DefaultPolicy{Plugins: plain}
	if _, _, err := s.InspectTargets(context.Background(), d, ts, time.Second, 2); err == nil {
		t.Error("atteso errore con un plugin senza ReleaseInspector")
	}
}
