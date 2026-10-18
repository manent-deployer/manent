package envconfig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/tenant"
)

func tenantEnv() EnvConfig {
	return EnvConfig{
		Plugin:  "remote-copy",
		Tenancy: TenancyTenanted,
		TenantParams: map[string]TenantParam{
			"db_host":   {Pattern: `[a-z0-9.-]+`, Required: true},
			"feature_x": {Pattern: `true|false`, Default: "false"},
		},
		Targets: []descriptor.Target{
			{Host: "shared", Labels: map[string]string{"tenant": "*"}},
			{Host: "acme-1", Labels: map[string]string{"tenant": "acme"}},
			{Host: "globex-1", Labels: map[string]string{"tenant": "globex"}},
		},
		Params:  map[string]string{"dest_dir": "/opt/{tenant}/app"},
		Secrets: []descriptor.SecretRef{{Name: "db", Ref: "vault:secret/data/myapp/{tenant}#db"}},
	}
}

func acme() *tenant.Tenant {
	return &tenant.Tenant{
		Name: "acme", Tags: map[string]string{"tier": "gold"},
		Applications: map[string][]string{"myapp": {"prod"}},
		Params:       map[string]map[string]string{"myapp": {"db_host": "acme-db.internal"}},
		Secrets:      map[string][]descriptor.SecretRef{"myapp": {{Name: "license", Ref: "vault:secret/data/acme#license"}}},
	}
}

func TestMergeTenant(t *testing.T) {
	d := baseDescriptor()
	d.Tenant = "acme"
	eff, err := MergeTenant(d, tenantEnv(), acme())
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, tg := range eff.Deploy.Targets {
		hosts = append(hosts, tg.Host)
	}
	if strings.Join(hosts, ",") != "shared,acme-1" {
		t.Errorf("target del tenant: %v", hosts)
	}
	p := eff.Deploy.Params
	if p["dest_dir"] != "/opt/acme/app" || p["db_host"] != "acme-db.internal" || p["feature_x"] != "false" {
		t.Errorf("parametri: %v", p)
	}
	if len(eff.Secrets) != 2 || eff.Secrets[0].Ref != "vault:secret/data/myapp/acme#db" || eff.Secrets[1].Name != "license" {
		t.Errorf("segreti: %+v", eff.Secrets)
	}

	cases := map[string]struct {
		mod  func(*descriptor.Descriptor, *EnvConfig, *tenant.Tenant)
		want string
	}{
		"senza tenant in un ambiente tenanted": {func(d *descriptor.Descriptor, _ *EnvConfig, _ *tenant.Tenant) { d.Tenant = "" }, "requires a tenant"},
		"tenant in un ambiente untenanted":     {func(_ *descriptor.Descriptor, c *EnvConfig, _ *tenant.Tenant) { c.Tenancy = "" }, "does not accept tenants"},
		"parametro obbligatorio mancante":      {func(_ *descriptor.Descriptor, _ *EnvConfig, tn *tenant.Tenant) { tn.Params = nil }, "db_host is required"},
		"parametro fuori pattern": {func(_ *descriptor.Descriptor, _ *EnvConfig, tn *tenant.Tenant) {
			tn.Params["myapp"]["db_host"] = "x;rm"
		}, "does not match"},
		"parametro non dichiarato": {func(_ *descriptor.Descriptor, _ *EnvConfig, tn *tenant.Tenant) {
			tn.Params["myapp"]["post_command"] = "x"
		}, "not declared"},
		"parametro del tenant nel descrittore": {func(d *descriptor.Descriptor, _ *EnvConfig, _ *tenant.Tenant) {
			d.Deploy.Params = map[string]string{"db_host": "x"}
		}, "descriptor cannot set it"},
		"segreto in conflitto": {func(_ *descriptor.Descriptor, _ *EnvConfig, tn *tenant.Tenant) {
			tn.Secrets["myapp"] = []descriptor.SecretRef{{Name: "db", Ref: "env:OTHER"}}
		}, "secrets.db differs"},
		"nessun target": {func(_ *descriptor.Descriptor, c *EnvConfig, _ *tenant.Tenant) { c.Targets = c.Targets[2:] }, "no target for tenant acme"},
	}
	for name, tc := range cases {
		d := baseDescriptor()
		d.Tenant = "acme"
		c, tn := tenantEnv(), acme()
		tc.mod(&d, &c, tn)
		if _, err := MergeTenant(d, c, tn); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: errore atteso con %q, ottenuto %v", name, tc.want, err)
		}
	}
	// Untenanted deploy in a "both" environment: no dedicated targets, {tenant} not allowed.
	c := tenantEnv()
	c.Tenancy, c.Params, c.Secrets = TenancyBoth, nil, nil
	eff, err = MergeTenant(baseDescriptor(), c, nil)
	if err != nil || len(eff.Deploy.Targets) != 1 || eff.Deploy.Targets[0].Host != "shared" {
		t.Errorf("deploy senza tenant: %+v %v", eff.Deploy.Targets, err)
	}
	if _, err := MergeTenant(baseDescriptor(), func() EnvConfig { c := tenantEnv(); c.Tenancy = TenancyBoth; return c }(), nil); err == nil || !strings.Contains(err.Error(), "uses {tenant}") {
		t.Errorf("segnaposto senza tenant: %v", err)
	}
}

func TestRegistroDeiTenantSuFile(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "myapp", "prod.yml"), "plugin: remote-copy\ntenancy: tenanted\ntenant_params: {db_host: {required: true}}\ntargets: [{host: h1, labels: {tenant: '*'}}]\n")
	write(filepath.Join(dir, "tenants", "acme.yml"), "tags: {tier: gold}\napplications: {myapp: [prod]}\nparams: {myapp: {db_host: acme-db}}\n")
	write(filepath.Join(dir, "tenants", "globex.yml"), "tags: {tier: silver}\napplications: {myapp: [test]}\n")
	write(filepath.Join(dir, "tenants", "rotto.yml"), "campo: 1\n")
	p := &FileProvider{PathTemplate: filepath.Join(dir, "{application}", "{environment}.yml"), TenantTemplate: filepath.Join(dir, "tenants", "{tenant}.yml")}
	if _, err := p.Tenants(context.Background()); err == nil {
		t.Error("documento non valido accettato")
	}
	_ = os.Remove(filepath.Join(dir, "tenants", "rotto.yml"))
	all, err := p.Tenants(context.Background())
	if err != nil || len(all) != 2 || all[0].Name != "acme" || all[0].Source == "" {
		t.Fatalf("elenco: %+v %v", all, err)
	}
	r := Resolver{Provider: p, Tenants: p}
	d := baseDescriptor()
	d.Tenant = "acme"
	eff, meta, err := r.Resolve(context.Background(), d)
	if err != nil || eff.Deploy.Params["db_host"] != "acme-db" || meta["tenant"] != "acme" || !strings.HasPrefix(meta["tenant_source"], "file:") {
		t.Fatalf("risoluzione: %+v %v %v", eff.Deploy.Params, meta, err)
	}
	d.Tenant = "globex"
	if _, _, err := r.Resolve(context.Background(), d); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("tenant non collegato: %v", err)
	}
	d.Tenant = "nessuno"
	if _, _, err := r.Resolve(context.Background(), d); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("tenant inesistente: %v", err)
	}
}

func TestRegistroDeiTenantSuConsul(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/kv/manent/tenants/" && r.URL.Query().Get("recurse") == "true":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Key": "manent/tenants/acme", "Value": enc("tags: {tier: gold}\n"), "ModifyIndex": 4},
				{"Key": "manent/tenants/globex", "Value": enc("tags: {tier: silver}\n"), "ModifyIndex": 9},
			})
		case r.URL.Path == "/v1/kv/manent/tenants/acme":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Key": "manent/tenants/acme", "Value": enc("tags: {tier: gold}\n"), "ModifyIndex": 4}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := &ConsulProvider{Address: srv.URL, TenantKeyTemplate: "manent/tenants/{tenant}"}
	all, err := p.Tenants(context.Background())
	if err != nil || len(all) != 2 || all[1].Name != "globex" || all[1].Revision != "9" {
		t.Fatalf("elenco: %+v %v", all, err)
	}
	if tn, err := p.Tenant(context.Background(), "acme"); err != nil || tn.Tags["tier"] != "gold" {
		t.Fatalf("tenant: %+v %v", tn, err)
	}
	if _, err := p.Tenant(context.Background(), "nessuno"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("tenant inesistente: %v", err)
	}
}
