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
		"proprietario doppio": "tenants:\n  a: {applications: {shop: {}}}\n  b: {applications: {shop: {}}}\n",
		"tag riservato":       "tenants:\n  a: {applications: {shop: {tags: {tenant: x}}}}\n",
		"prefisso riservato":  "tenants:\n  a: {applications: {shop: {tags: {tenant.tier: x}}}}\n",
		"nome del tenant":     "tenants:\n  A_B: {}\n",
		"nome applicazione":   "tenants:\n  a: {applications: {\"team/api\": {}}}\n",
		"campo sconosciuto":   "tenants:\n  a: {params: {}}\n",
		"concorrenza":         "tenants:\n  a: {concurrency: -1}\n",
	}
	for name, doc := range cases {
		if _, err := ParseRegistry([]byte(doc)); err == nil {
			t.Errorf("%s: registro accettato", name)
		}
	}
	_, err := ParseRegistry([]byte(cases["proprietario doppio"]))
	if err == nil || !strings.Contains(err.Error(), "belongs to two tenants") {
		t.Errorf("errore della proprietà doppia: %v", err)
	}
}

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
	owner, err := Owner(ctx, c, "billing")
	if err != nil || owner.Name != "payments" {
		t.Fatalf("proprietario di billing: %+v %v", owner, err)
	}
	if _, err := Owner(ctx, c, "unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("applicazione non registrata: %v", err)
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
	if _, err := Owner(ctx, c, "shop"); err != nil {
		t.Errorf("con il registro illeggibile vale l'ultimo elenco: %v", err)
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
