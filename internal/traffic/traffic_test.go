package traffic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
)

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", errors.New("segreto assente")
}

// fakeDataplane emulates the Data Plane API v3 of an HAProxy instance.
type fakeDataplane struct {
	mu      sync.Mutex
	servers map[string]map[string]any
	scur    map[string]int
	puts    []string
}

func newFakeDataplane() *fakeDataplane {
	f := &fakeDataplane{servers: map[string]map[string]any{}, scur: map[string]int{}}
	for _, n := range []string{"web1", "web2", "web3"} {
		f.servers[n] = map[string]any{"name": n, "admin_state": "ready", "operational_state": "up"}
	}
	return f
}

func (f *fakeDataplane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "s3cret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const base = "/v3/services/haproxy/runtime/backends/shop/servers"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		var list []map[string]any
		for _, n := range []string{"web1", "web2", "web3"} {
			list = append(list, f.servers[n])
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, base+"/"):
		name := strings.TrimPrefix(r.URL.Path, base+"/")
		s, ok := f.servers[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		s["admin_state"] = in["admin_state"]
		f.puts = append(f.puts, name+"="+in["admin_state"])
		_ = json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodGet && r.URL.Path == "/v3/services/haproxy/stats/native":
		name := r.URL.Query().Get("name")
		_ = json.NewEncoder(w).Encode(map[string]any{"runtimeAPI": "x", "stats": []any{
			map[string]any{"name": name, "type": "server", "backend_name": "shop", "stats": map[string]any{"scur": f.scur[name], "status": "UP"}},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestHAProxyPool(t *testing.T) {
	f1, f2 := newFakeDataplane(), newFakeDataplane()
	s1, s2 := httptest.NewServer(f1), httptest.NewServer(f2)
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	settings := map[string]string{"dataplane_urls": s1.URL + "," + s2.URL, "backend": "shop", "credentials_secret": "dp", "allow_http": "true"}
	if err := Default().Check(&descriptor.TrafficConfig{Pool: &descriptor.PoolConfig{Provider: "haproxy", Settings: settings}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := NewHAProxyPool(ctx, Env{Settings: settings, Secrets: mapSecrets{"dp": "admin:s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	// A server down on a single instance is not healthy for the pool.
	f2.servers["web3"]["operational_state"] = "down"
	all, err := p.Members(ctx)
	if err != nil || len(all) != 3 || all[2].InService() {
		t.Fatalf("membri: %+v %v", all, err)
	}
	// With web3 out of service, draining web1 would leave a single healthy member out of 3.
	if err := Capacity(all, []string{"web1"}, 2); err == nil {
		t.Error("capacità insufficiente non rilevata")
	}
	if err := Capacity(all, []string{"web1"}, 1); err != nil {
		t.Error(err)
	}
	if err := p.Drain(ctx, []string{"web1"}); err != nil {
		t.Fatal(err)
	}
	if f1.servers["web1"]["admin_state"] != "drain" || f2.servers["web1"]["admin_state"] != "drain" {
		t.Error("svuotamento non applicato a tutte le istanze")
	}
	f1.scur["web1"], f2.scur["web1"] = 2, 1
	if c, err := p.Connections(ctx, []string{"web1"}); err != nil || c["web1"] != 3 {
		t.Errorf("connessioni: %v %v", c, err)
	}
	if err := p.Restore(ctx, []string{"web1"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := p.Healthy(ctx, []string{"web1", "web3"}); err != nil || !ok["web1"] || ok["web3"] {
		t.Errorf("salute: %v %v", ok, err)
	}
	if strings.Join(f1.puts, ",") != "web1=drain,web1=ready" {
		t.Errorf("chiamate: %v", f1.puts)
	}
	// v2 statistics are a list, one per process.
	n, err := haproxySessions([]byte(`[{"stats":[{"name":"web1","type":"server","stats":{"scur":4}}]},{"stats":[{"name":"web1","type":"server","stats":{"scur":1}}]}]`), "web1")
	if err != nil || n != 5 {
		t.Errorf("statistiche v2: %d %v", n, err)
	}
}

func TestHAProxyValidazione(t *testing.T) {
	err := validateHAProxy(map[string]string{"dataplane_urls": "http://lb:5555"})
	if err == nil || !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "backend") {
		t.Errorf("validazione: %v", err)
	}
	if err := Default().Check(&descriptor.TrafficConfig{Pool: &descriptor.PoolConfig{Provider: "nessuno"}}); err == nil {
		t.Error("provider sconosciuto accettato")
	}
}

func TestHealthFlagHTTP(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	draining := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/admin/drain":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			calls = append(calls, r.Method)
			draining[r.Host] = r.Method == http.MethodPut
		case "/healthz":
			if draining[r.Host] {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	settings := map[string]string{"drain_url": "http://{host}/admin/drain", "health_url": "http://{host}/healthz", "token_secret": "tok",
		"allow_http": "true", "drain_delay": "1s"}
	if err := validateHealthFlag(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	targets := []descriptor.Target{{Host: host}}
	p, err := NewHealthFlagPool(ctx, Env{Settings: settings, Targets: targets, Secrets: mapSecrets{"tok": "tok"}, Member: (&descriptor.PoolConfig{}).MemberName})
	if err != nil {
		t.Fatal(err)
	}
	if dd := p.(DrainDelayer).DrainDelay(); dd.Seconds() != 1 {
		t.Errorf("attesa: %s", dd)
	}
	if err := p.Drain(ctx, []string{host}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Healthy(ctx, []string{host}); ok[host] {
		t.Error("target svuotato ancora sano")
	}
	if err := p.Restore(ctx, []string{host}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Healthy(ctx, []string{host}); !ok[host] {
		t.Error("target rientrato non sano")
	}
	if strings.Join(calls, ",") != "PUT,DELETE" {
		t.Errorf("chiamate: %v", calls)
	}
	if all, _ := p.Members(ctx); all != nil {
		t.Error("health-flag non conosce gli altri membri")
	}
}

func TestMinHealthyEMember(t *testing.T) {
	p := &descriptor.PoolConfig{MinHealthy: "66%", Member: "{label.lb}:{port}"}
	if n := p.MinHealthyFor(3); n != 2 {
		t.Errorf("66%% di 3: %d", n)
	}
	if n := (&descriptor.PoolConfig{}).MinHealthyFor(4); n != 1 {
		t.Errorf("default: %d", n)
	}
	if m := p.MemberName(descriptor.Target{Host: "h", Port: 8080, Labels: map[string]string{"lb": "web1"}}); m != "web1:8080" {
		t.Errorf("membro: %s", m)
	}
}
