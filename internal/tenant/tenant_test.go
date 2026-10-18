package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func registry() []Tenant {
	return []Tenant{
		{Name: "acme", Tags: map[string]string{"tier": "gold", "region": "eu"}, Applications: map[string][]string{"shop": {"prod"}}},
		{Name: "globex", Tags: map[string]string{"tier": "silver", "region": "eu"}, Applications: map[string][]string{"shop": {"prod", "test"}}},
		{Name: "initech", Tags: map[string]string{"tier": "gold", "region": "us"}, Applications: map[string][]string{"shop": {"prod"}}, Disabled: true},
		{Name: "umbrella", Tags: map[string]string{"tier": "gold"}, Applications: map[string][]string{"billing": {"prod"}}},
	}
}

func TestSelezione(t *testing.T) {
	got, err := Select(registry(), "shop", "prod", nil, "tier in (gold, silver), region=eu")
	if err != nil || len(got) != 2 || got[0].Name != "acme" || got[1].Name != "globex" {
		t.Fatalf("selettore: %+v %v", got, err)
	}
	if got, _ := Select(registry(), "shop", "prod", nil, "tier=gold"); len(got) != 1 {
		t.Errorf("disabilitati e non collegati esclusi: %+v", got)
	}
	if got, _ := Select(registry(), "shop", "prod", nil, "name=globex"); len(got) != 1 || got[0].Name != "globex" {
		t.Errorf("selettore sul nome: %+v", got)
	}
	for name, tc := range map[string]struct {
		names []string
		sel   string
		want  string
	}{
		"entrambi":         {[]string{"acme"}, "tier=gold", "either names or selector"},
		"nessuno":          {nil, "", "either names or selector"},
		"inesistente":      {[]string{"nessuno"}, "", "not found"},
		"disabilitato":     {[]string{"initech"}, "", "disabled"},
		"non collegato":    {[]string{"umbrella"}, "", "not connected"},
		"nessun risultato": {nil, "tier=bronze", "matches"},
	} {
		if _, err := Select(registry(), "shop", "prod", tc.names, tc.sel); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: errore atteso con %q, ottenuto %v", name, tc.want, err)
		}
	}
}

func TestCorrispondenzaESelettori(t *testing.T) {
	r := &Ref{Name: "acme", Tags: map[string]string{"tier": "gold"}}
	if !Matches("", nil) || !Matches("", r) {
		t.Error("il selettore vuoto vale per tutti")
	}
	if Matches("tier=gold", nil) {
		t.Error("un selettore non vuoto non vale per i deploy senza tenant")
	}
	if !Matches("tier=gold", r) || Matches("tier=silver", r) || !Matches("name=acme", r) {
		t.Error("corrispondenza dei tag")
	}
	if CheckSelector("tier==") == nil {
		t.Error("selettore non valido accettato")
	}
	if _, err := Parse([]byte("tags: {name: x}\n"), "acme"); err == nil {
		t.Error("tag riservato accettato")
	}
	if _, err := Parse([]byte("name: other\n"), "acme"); err == nil {
		t.Error("nome diverso dal documento accettato")
	}
}

type countDir struct{ n int }

func (c *countDir) Tenant(context.Context, string) (*Tenant, error) { return nil, ErrNotFound }
func (c *countDir) Tenants(context.Context) ([]Tenant, error) {
	c.n++
	if c.n > 1 {
		return nil, errors.New("irraggiungibile")
	}
	return registry(), nil
}

func TestCache(t *testing.T) {
	next := &countDir{}
	c := &Cache{Next: next, TTL: time.Hour}
	if tn, err := c.Tenant(context.Background(), "globex"); err != nil || tn.Tags["tier"] != "silver" {
		t.Fatal(tn, err)
	}
	_, _ = c.Tenants(context.Background())
	if next.n != 1 {
		t.Errorf("letture del registro: %d", next.n)
	}
	c.TTL = 0
	if l, err := c.Tenants(context.Background()); err != nil || len(l) != 4 {
		t.Errorf("con il registro irraggiungibile resta l'ultimo elenco: %v %v", l, err)
	}
	if _, err := c.Tenant(context.Background(), "nessuno"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tenant inesistente: %v", err)
	}
}
