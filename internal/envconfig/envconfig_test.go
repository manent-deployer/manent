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
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
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
	srv := fakeConsul(t, "manent/myapp/prod", yamlCfg, "tok", "dc1")
	p := &ConsulProvider{Address: srv.URL, Token: "tok", Datacenter: "dc1", KeyTemplate: "manent/{application}/{environment}"}
	ctx := context.Background()

	doc, err := p.Get(ctx, "myapp", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source != "consul:manent/myapp/prod" || doc.Revision != "42" || doc.Config.Targets[0].Host != "h1" {
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
	srv := fakeConsul(t, "manent/myapp/prod", "plugin: x\ncampo_sconosciuto: 1\n", "", "")
	p := &ConsulProvider{Address: srv.URL, KeyTemplate: "manent/{application}/{environment}"}
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

func TestMergeRollback(t *testing.T) {
	on, off := true, false
	base := descriptor.Descriptor{Deploy: descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}}}

	// L'ambiente impone il rollback; il descrittore aggiunge gli step.
	d := base
	d.Rollback = &descriptor.Rollback{On: []string{"post-test"}}
	eff, err := Merge(d, EnvConfig{Rollback: &descriptor.Rollback{Enabled: &on, Locator: "cmdb"}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Rollback.IsEnabled() || eff.Rollback.Locator != "cmdb" || !eff.Rollback.Triggers("post-test") {
		t.Errorf("rollback effettivo inatteso: %+v", eff.Rollback)
	}

	// Il descrittore non può disattivare un rollback imposto dall'ambiente.
	d.Rollback = &descriptor.Rollback{Enabled: &off}
	if _, err := Merge(d, EnvConfig{Rollback: &descriptor.Rollback{Enabled: &on}}); err == nil {
		t.Error("atteso conflitto su rollback.enabled")
	}

	// Senza rollback nell'ambiente il descrittore resta invariato.
	eff, err = Merge(d, EnvConfig{})
	if err != nil || eff.Rollback.IsEnabled() {
		t.Errorf("rollback inatteso: %+v %v", eff.Rollback, err)
	}
}

func TestDescriptorPolicy(t *testing.T) {
	pol := &DescriptorPolicy{
		Params:     map[string][]string{"*": {"verify_checksum"}, "remote-copy": {"dest_dir", "keep_*"}},
		SecretRefs: []string{"vault:secret/data/{application}/*"},
	}
	d := baseDescriptor()
	d.Deploy.Plugin = "remote-copy"
	d.Deploy.Params = map[string]string{"dest_dir": "/opt/a", "keep_releases": "3", "verify_checksum": "false"}
	d.Secrets = []descriptor.SecretRef{{Name: "key", Ref: "vault:secret/data/myapp/prod#ssh"}}
	env := EnvConfig{Targets: []descriptor.Target{{Host: "h1"}}}
	if err := pol.Check(d, env); err != nil {
		t.Fatalf("descrittore ammesso rifiutato: %v", err)
	}

	// Comandi, segreti di altre applicazioni e target: non ammessi dal descrittore.
	d.Deploy.Params["post_command"] = "rm -rf /"
	d.Secrets = append(d.Secrets, descriptor.SecretRef{Name: "altro", Ref: "vault:secret/data/other/prod#ssh"}, descriptor.SecretRef{Name: "env", Ref: "env:CONSUL_HTTP_TOKEN"})
	d.Deploy.Targets = []descriptor.Target{{Host: "evil"}}
	err := pol.Check(d, EnvConfig{})
	for _, want := range []string{"deploy.params.post_command", "secrets.altro", "secrets.env", "deploy.targets"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso errore su %s, ricevuto %v", want, err)
		}
	}

	// Ciò che la configurazione di ambiente definisce non è soggetto alla policy.
	env = EnvConfig{
		Params:  map[string]string{"post_command": "rm -rf /"},
		Secrets: []descriptor.SecretRef{{Name: "altro", Ref: "vault:secret/data/other/prod#ssh"}, {Name: "env", Ref: "env:CONSUL_HTTP_TOKEN"}},
		Targets: []descriptor.Target{{Host: "evil"}},
	}
	if err := pol.Check(d, env); err != nil {
		t.Errorf("valori della configurazione di ambiente rifiutati: %v", err)
	}

	// Policy assente: nessun limite.
	if err := (*DescriptorPolicy)(nil).Check(d, EnvConfig{}); err != nil {
		t.Error(err)
	}
}

func TestMergeRollout(t *testing.T) {
	c, err := Parse([]byte(`
targets:
  - {host: h1, labels: {region: eu-1, az: eu-1a}}
rollout:
  waves:
    - {name: tutto, select: "region=eu-1", by: az, bake: 5m}
  onFailure: stop
`))
	if err != nil {
		t.Fatal(err)
	}
	d := descriptor.Descriptor{Application: "app", Environment: "prod"}
	got, err := Merge(d, c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rollout == nil || got.Rollout.Waves[0].Bake != "5m" || got.Deploy.Targets[0].Labels["az"] != "eu-1a" {
		t.Fatalf("fusione: %+v", got)
	}
	// Lo stesso rollout ripetuto nel descrittore è ammesso, uno diverso no.
	d.Rollout = c.Rollout
	if _, err := Merge(d, c); err != nil {
		t.Fatalf("rollout identico rifiutato: %v", err)
	}
	other := *c.Rollout
	other.OnFailure = "rollback-all"
	d.Rollout = &other
	if _, err := Merge(d, c); err == nil {
		t.Fatal("rollout diverso dal descrittore accettato")
	}
	// Target con etichette diverse: conflitto.
	d.Rollout = nil
	d.Deploy.Targets = []descriptor.Target{{Host: "h1", Labels: map[string]string{"region": "us-1"}}}
	if _, err := Merge(d, c); err == nil {
		t.Fatal("target con etichette diverse accettati")
	}
}

func TestStepParams(t *testing.T) {
	c, err := Parse([]byte(`
targets: [{host: h1}]
step_params:
  db-migrate: {database: shop}
  cdn-purge: {zone: abc}
`))
	if err != nil {
		t.Fatal(err)
	}
	d := baseDescriptor()
	d.StepParams = map[string]map[string]string{"db-migrate": {"database": "shop", "mode": "safe"}}
	eff, err := Merge(d, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.StepParams; got["db-migrate"]["database"] != "shop" || got["db-migrate"]["mode"] != "safe" || got["cdn-purge"]["zone"] != "abc" {
		t.Errorf("parametri fusi: %v", got)
	}
	if d.StepParams["db-migrate"]["database"] != "shop" || len(d.StepParams) != 1 {
		t.Error("la fusione ha modificato il descrittore originale")
	}
	d.StepParams["db-migrate"]["database"] = "other"
	if _, err := Merge(d, c); err == nil || !strings.Contains(err.Error(), "stepParams.db-migrate.database") {
		t.Errorf("conflitto non rilevato: %v", err)
	}

	// Il descrittore imposta solo i parametri ammessi dalla policy; quelli dell'ambiente no.
	pol := &DescriptorPolicy{StepParams: map[string][]string{"db-migrate": {"mode"}}}
	d.StepParams = map[string]map[string]string{"db-migrate": {"database": "shop", "mode": "safe"}}
	if err := pol.Check(d, c); err != nil {
		t.Errorf("parametri ammessi rifiutati: %v", err)
	}
	d.StepParams["cdn-purge"] = map[string]string{"zone": "abc", "extra": "1"}
	d.StepParams["db-migrate"]["dsn"] = "x"
	err = pol.Check(d, EnvConfig{})
	for _, want := range []string{"stepParams.cdn-purge.extra", "stepParams.cdn-purge.zone", "stepParams.db-migrate.database", "stepParams.db-migrate.dsn"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso errore su %s: %v", want, err)
		}
	}
	if err := (&DescriptorPolicy{}).Check(descriptor.Descriptor{StepParams: map[string]map[string]string{"x": {"a": "1"}}}, EnvConfig{}); err == nil {
		t.Error("senza step_params nella policy il descrittore non può impostare parametri")
	}
}

func TestMergeTraffic(t *testing.T) {
	d := descriptor.Descriptor{Application: "shop", Environment: "prod", Deploy: descriptor.Deploy{Targets: []descriptor.Target{{Host: "h1"}}}}
	cfg := EnvConfig{Traffic: &descriptor.TrafficConfig{Pool: &descriptor.PoolConfig{Provider: "haproxy"}}}
	eff, err := Merge(d, cfg)
	if err != nil || eff.Traffic == nil || eff.Traffic.Pool.Provider != "haproxy" {
		t.Fatalf("fusione: %+v %v", eff.Traffic, err)
	}
	// Il descrittore non può definire load balancer e router.
	d.Traffic = &descriptor.TrafficConfig{Pool: &descriptor.PoolConfig{Provider: "haproxy"}}
	if _, err := Merge(d, cfg); err == nil || !strings.Contains(err.Error(), "solo nella configurazione di ambiente") {
		t.Errorf("traffic nel descrittore: %v", err)
	}
	cfg.Traffic.Pool.OnTimeout = "boh"
	d.Traffic = nil
	if _, err := Merge(d, cfg); err == nil || !strings.Contains(err.Error(), "on_timeout") {
		t.Errorf("configurazione non valida: %v", err)
	}
	if _, err := Parse([]byte("traffic:\n  pool:\n    provider: aws-elb\n    settings: {region: eu-west-1}\n")); err != nil {
		t.Errorf("parse: %v", err)
	}
}
