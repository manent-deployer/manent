package tenant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const registryYAML = `
tenants:
  payments:
    description: Team pagamenti
    tags: {division: commerce}
    applications:
      shop: {tags: {product: checkout, criticality: high}}
      billing: {tags: {product: invoicing}}
    members: {groups: [payments-eng]}
    admins: {subjects: ["oidc:alice"]}
    queue: deployments.payments
    concurrency: 2
  platform:
    applications:
      infra: {}
`

func TestRegistro(t *testing.T) {
	ts, err := ParseRegistry([]byte(registryYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Name != "payments" || ts[1].Name != "platform" {
		t.Fatalf("registro inatteso: %+v", ts)
	}
	p := ts[0]
	if !p.Owns("shop") || p.Owns("infra") || p.Queue != "deployments.payments" || p.Members.Groups[0] != "payments-eng" {
		t.Errorf("tenant payments inatteso: %+v", p)
	}
	if ts, err := ParseRegistry(nil); err != nil || len(ts) != 0 {
		t.Errorf("un registro vuoto è valido: %v %v", ts, err)
	}
}

func TestRegistroNonValido(t *testing.T) {
	cases := map[string]string{
		"nome riservato":     "tenants:\n  untenanted: {applications: {shop: {}}}\n",
		"tag riservato":      "tenants:\n  a: {applications: {shop: {tags: {tenant: x}}}}\n",
		"prefisso riservato": "tenants:\n  a: {applications: {shop: {tags: {tenant.tier: x}}}}\n",
		"nome del tenant":    "tenants:\n  A_B: {}\n",
		"nome applicazione":  "tenants:\n  a: {applications: {\"team/api\": {}}}\n",
		"campo sconosciuto":  "tenants:\n  a: {params: {}}\n",
		"concorrenza":        "tenants:\n  a: {concurrency: -1}\n",
	}
	for name, doc := range cases {
		if _, err := ParseRegistry([]byte(doc)); err == nil {
			t.Errorf("%s: registro accettato", name)
		}
	}
	_, err := ParseRegistry([]byte(cases["nome riservato"]))
	if err == nil || !strings.Contains(err.Error(), "is reserved") {
		t.Errorf("errore del nome riservato: %v", err)
	}
	// Two tenants can list applications with the same name.
	if _, err := ParseRegistry([]byte("tenants:\n  a: {applications: {shop: {}}}\n  b: {applications: {shop: {}}}\n")); err != nil {
		t.Errorf("stesso nome in due tenant: %v", err)
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	reg := `
tenants:
  payments: {applications: {shop: {}, billing: {}}}
  retail: {applications: {shop: {}}}
  archive: {disabled: true, applications: {old: {}}}
`
	dir := staticDir(reg)
	for _, c := range []struct {
		app, chosen string
		allow       bool
		want, err   string
	}{
		{"billing", "", true, "payments", ""},
		{"shop", "", true, "", "belongs to the tenants payments, retail"},
		{"shop", "retail", true, "retail", ""},
		{"shop", "platform", true, "", "tenant platform does not list application shop"},
		{"shop", Untenanted, true, "", "it is not untenanted"},
		{"other", "", true, Untenanted, ""},
		{"other", Untenanted, true, Untenanted, ""},
		{"other", "", false, "", "not registered in any tenant"},
		{"other", "payments", true, "", "does not list"},
		{"old", "", true, "", "is disabled"},
	} {
		got, err := Resolve(ctx, dir, c.app, c.chosen, c.allow)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s/%q: errore %v, atteso %q", c.app, c.chosen, err, c.err)
		case c.err == "" && (err != nil || got.Name != c.want):
			t.Errorf("%s/%q: %+v %v, atteso %s", c.app, c.chosen, got, err, c.want)
		}
	}
	if !errors.Is(func() error { _, err := Resolve(ctx, dir, "shop", "", true); return err }(), ErrAmbiguous) {
		t.Error("ambiguità non riconosciuta")
	}
	// Without a registry every application is untenanted.
	if got, err := Resolve(ctx, nil, "shop", "", true); err != nil || !got.IsUntenanted() {
		t.Errorf("senza registro: %+v %v", got, err)
	}
}

func TestPatternQualificati(t *testing.T) {
	for _, c := range []struct {
		pattern, tenant, app string
		want                 bool
	}{
		{"shop", Untenanted, "shop", true},
		{"shop", "payments", "shop", false},
		{"*", "payments", "shop", false},
		{"*", "", "shop", true},
		{"payments/shop", "payments", "shop", true},
		{"payments/*", "payments", "billing", true},
		{"payments/*", "retail", "shop", false},
		{"*/shop", "retail", "shop", true},
		{"*/*", Untenanted, "x", true},
	} {
		if got := AppMatches(c.pattern, c.tenant, c.app); got != c.want {
			t.Errorf("AppMatches(%q, %q, %q) = %v", c.pattern, c.tenant, c.app, got)
		}
	}
	for _, bad := range []string{"", "/shop", "payments/", "a/b/c"} {
		if CheckAppPattern(bad) == nil {
			t.Errorf("pattern %q accettato", bad)
		}
	}
}

// staticDir is a registry parsed from a YAML document.
type staticDir string

func (d staticDir) Tenants(context.Context) ([]Tenant, error) { return ParseRegistry([]byte(d)) }

func TestSelettori(t *testing.T) {
	ts, _ := ParseRegistry([]byte(registryYAML))
	r := ts[0].Ref("shop")
	for sel, want := range map[string]bool{
		"":                    true,
		"name=payments":       true,
		"division=commerce":   true,
		"name=platform":       false,
		"criticality=high":    false, // an application tag: not a tenant label
		"tier in (gold)":      false,
		"invalid selector ==": false,
	} {
		if got := Matches(sel, r); got != want {
			t.Errorf("Matches(%q) = %v", sel, got)
		}
	}
	for sel, want := range map[string]bool{
		"criticality=high":                      true,
		"application=shop":                      true,
		"tenant=payments":                       true,
		"tenant.division=commerce":              true,
		"product in (checkout, invoicing)":      true,
		"product=invoicing":                     false,
		"criticality=high, tenant.division=ops": false,
	} {
		if got := MatchesApp(sel, r); got != want {
			t.Errorf("MatchesApp(%q) = %v", sel, got)
		}
	}
	if Matches("name=payments", nil) || !Matches("", nil) || MatchesApp("application=shop", nil) {
		t.Error("senza tenant vale solo il selettore vuoto")
	}
}

// countingDir counts the listings.
type countingDir struct {
	n   int
	err error
}

func (c *countingDir) Tenants(context.Context) ([]Tenant, error) {
	c.n++
	if c.err != nil {
		return nil, c.err
	}
	return ParseRegistry([]byte(registryYAML))
}

func TestCacheEProprietario(t *testing.T) {
	ctx := context.Background()
	next := &countingDir{}
	c := &Cache{Next: next, TTL: time.Hour}
	owners, err := Owners(ctx, c, "billing")
	if err != nil || len(owners) != 1 || owners[0].Name != "payments" {
		t.Fatalf("proprietario di billing: %+v %v", owners, err)
	}
	if owners, _ := Owners(ctx, c, "unknown"); len(owners) != 0 {
		t.Errorf("applicazione non registrata: %v", owners)
	}
	if tn, err := Find(ctx, c, "platform"); err != nil || !tn.Owns("infra") {
		t.Errorf("Find: %+v %v", tn, err)
	}
	if next.n != 1 {
		t.Errorf("letture del registro: %d, attesa 1", next.n)
	}
	// An unreadable registry keeps the last listing.
	c.TTL = 0
	next.err = errors.New("giù")
	var warned []error
	c.OnError = func(err error) { warned = append(warned, err) }
	if owners, err := Owners(ctx, c, "shop"); err != nil || len(owners) != 1 || c.Err() == nil {
		t.Errorf("con il registro illeggibile vale l'ultimo elenco: %v", err)
	}
	_, _ = c.Tenants(ctx)
	if len(warned) != 1 {
		t.Errorf("un avviso per errore distinto: %v", warned)
	}
	next.err = nil
	if _, _ = c.Tenants(ctx); c.Err() != nil {
		t.Errorf("un registro di nuovo valido azzera l'errore: %v", c.Err())
	}
}

func TestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yml")
	if err := os.WriteFile(path, []byte(registryYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := File{Path: path}.Tenants(context.Background())
	if err != nil || len(ts) != 2 || ts[0].Source != "file:"+path || ts[0].Revision == "" {
		t.Fatalf("lettura del file: %+v %v", ts, err)
	}
	if _, err := (File{Path: path + ".missing"}).Tenants(context.Background()); err == nil {
		t.Error("file mancante accettato")
	}
}
