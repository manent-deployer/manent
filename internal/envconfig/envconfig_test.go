package envconfig

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/joeg-ita/go-dart/internal/descriptor"
)

func baseDescriptor() descriptor.Descriptor {
	return descriptor.Descriptor{Application: "myapp", Environment: "prod"}
}

func envCfg() EnvConfig {
	return EnvConfig{
		Plugin:  "remote-copy",
		Targets: []descriptor.Target{{Host: "h1", User: "deploy"}},
		Params:  map[string]string{"dest_dir": "/opt/app"},
		Secrets: []descriptor.SecretRef{{Name: "key", Ref: "env:KEY"}},
	}
}

func TestMerge(t *testing.T) {
	t.Run("solo configurazione di ambiente", func(t *testing.T) {
		eff, err := Merge(baseDescriptor(), envCfg())
		if err != nil {
			t.Fatal(err)
		}
		if eff.Deploy.Plugin != "remote-copy" || eff.Deploy.Targets[0].Host != "h1" || eff.Deploy.Params["dest_dir"] != "/opt/app" || len(eff.Secrets) != 1 {
			t.Errorf("descrittore effettivo inatteso: %+v", eff)
		}
	})
	t.Run("il descrittore aggiunge parametri e segreti", func(t *testing.T) {
		d := baseDescriptor()
		d.Deploy.Params = map[string]string{"post_command": "restart", "dest_dir": "/opt/app"}
		d.Secrets = []descriptor.SecretRef{{Name: "key", Ref: "env:KEY"}, {Name: "extra", Ref: "env:X"}}
		eff, err := Merge(d, envCfg())
		if err != nil {
			t.Fatal(err)
		}
		if eff.Deploy.Params["post_command"] != "restart" || len(eff.Secrets) != 2 {
			t.Errorf("descrittore effettivo inatteso: %+v", eff)
		}
		if d.Deploy.Params["dest_dir"] != "/opt/app" || len(d.Deploy.Params) != 2 {
			t.Error("Merge non deve modificare il descrittore di partenza")
		}
	})
	t.Run("solo descrittore", func(t *testing.T) {
		d := baseDescriptor()
		d.Deploy.Targets = []descriptor.Target{{Host: "h2"}}
		if _, err := Merge(d, EnvConfig{}); err != nil {
			t.Fatal(err)
		}
	})

	conflicts := map[string]func(*descriptor.Descriptor){
		"plugin diverso":    func(d *descriptor.Descriptor) { d.Deploy.Plugin = "altro" },
		"target diversi":    func(d *descriptor.Descriptor) { d.Deploy.Targets = []descriptor.Target{{Host: "h9"}} },
		"parametro diverso": func(d *descriptor.Descriptor) { d.Deploy.Params = map[string]string{"dest_dir": "/tmp"} },
		"segreto diverso":   func(d *descriptor.Descriptor) { d.Secrets = []descriptor.SecretRef{{Name: "key", Ref: "env:ALTRO"}} },
	}
	for name, mutate := range conflicts {
		t.Run(name, func(t *testing.T) {
			d := baseDescriptor()
			mutate(&d)
			if _, err := Merge(d, envCfg()); err == nil {
				t.Fatal("atteso errore di conflitto")
			}
		})
	}
	t.Run("nessun target", func(t *testing.T) {
		if _, err := Merge(baseDescriptor(), EnvConfig{}); err == nil {
			t.Fatal("atteso errore: nessun target")
		}
	})
}

// fakeConsul simula l'API KV di Consul per una singola chiave.
func fakeConsul(t *testing.T, key, value string, wantToken, wantDC string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Consul-Token") != wantToken {
			http.Error(w, "ACL not found", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("dc") != wantDC {
			http.Error(w, "dc errato", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/kv/"+key {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `[{"Key":%q,"Value":%q,"ModifyIndex":42}]`, key, base64.StdEncoding.EncodeToString([]byte(value)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConsulProvider(t *testing.T) {
	yamlCfg := "plugin: remote-copy\ntargets:\n  - {host: h1, user: deploy}\nparams:\n  dest_dir: /opt/app\n"
	srv := fakeConsul(t, "go-dart/myapp/prod", yamlCfg, "tok", "dc1")
	p := &ConsulProvider{Address: srv.URL, Token: "tok", Datacenter: "dc1", KeyTemplate: "go-dart/{application}/{environment}"}
	ctx := context.Background()

	doc, err := p.Get(ctx, "myapp", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source != "consul:go-dart/myapp/prod" || doc.Revision != "42" || doc.Config.Targets[0].Host != "h1" {
		t.Errorf("documento inatteso: %+v", doc)
	}
	if _, err := p.Get(ctx, "myapp", "test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("atteso ErrNotFound, ricevuto %v", err)
	}
	p.Token = "sbagliato"
	if _, err := p.Get(ctx, "myapp", "prod"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("atteso errore di autorizzazione, ricevuto %v", err)
	}
}

func TestConsulProviderValoreNonValido(t *testing.T) {
	srv := fakeConsul(t, "go-dart/myapp/prod", "plugin: x\ncampo_sconosciuto: 1\n", "", "")
	p := &ConsulProvider{Address: srv.URL, KeyTemplate: "go-dart/{application}/{environment}"}
	if _, err := p.Get(context.Background(), "myapp", "prod"); err == nil {
		t.Fatal("atteso errore per campo sconosciuto")
	}
}

func TestFileProviderEResolver(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "myapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"plugin": "remote-copy", "targets": [{"host": "h1"}], "params": {"dest_dir": "/opt"}}`
	if err := os.WriteFile(filepath.Join(dir, "myapp", "prod.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &FileProvider{PathTemplate: filepath.Join(dir, "{application}", "{environment}.json")}
	ctx := context.Background()

	eff, meta, err := Resolver{Provider: p, Required: true}.Resolve(ctx, baseDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	if eff.Deploy.Params["dest_dir"] != "/opt" || meta["source"] != "file:"+filepath.Join(dir, "myapp", "prod.json") {
		t.Errorf("risoluzione inattesa: %+v %v", eff.Deploy, meta)
	}

	missing := baseDescriptor()
	missing.Environment = "test"
	if _, _, err := (Resolver{Provider: p, Required: true}).Resolve(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("con Required atteso ErrNotFound, ricevuto %v", err)
	}
	missing.Deploy.Targets = []descriptor.Target{{Host: "h2"}}
	if _, _, err := (Resolver{Provider: p}).Resolve(ctx, missing); err != nil {
		t.Errorf("senza Required la configurazione assente è ammessa: %v", err)
	}
}

func TestEsempio(t *testing.T) {
	p := &FileProvider{PathTemplate: "../../examples/envconfig/{application}/{environment}.yml"}
	doc, err := p.Get(context.Background(), "myapp", "test")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.Plugin != "remote-copy" || len(doc.Config.Targets) != 1 || len(doc.Config.Secrets) != 1 {
		t.Errorf("esempio inatteso: %+v", doc.Config)
	}
}
