package traffic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeBigIP emula iControl REST per un pool con due membri.
type fakeBigIP struct {
	mu      sync.Mutex
	members map[string]map[string]any
	conns   map[string]int
	syncs   []string
}

func (f *fakeBigIP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const base = "/mgmt/tm/ltm/pool/~Common~shop/members"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		var items []any
		for _, n := range []string{"10.0.0.1:80", "10.0.0.2:80"} {
			items = append(items, f.members[n])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stats"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/~Common~"), "/stats")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": map[string]any{
			"https://localhost" + base + "/~Common~" + name + "/stats": map[string]any{"nestedStats": map[string]any{"entries": map[string]any{
				"serverside.curConns": map[string]any{"value": f.conns[name]},
			}}},
		}})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/~Common~"):
		m := f.members[strings.TrimPrefix(r.URL.Path, base+"/~Common~")]
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["session"] == "user-disabled" {
			m["session"] = "user-disabled"
		} else {
			m["session"], m["state"] = "monitor-enabled", "up"
		}
		_ = json.NewEncoder(w).Encode(m)
	case r.Method == http.MethodPost && r.URL.Path == "/mgmt/tm/cm":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.syncs = append(f.syncs, in["utilCmdArgs"])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestF5Pool(t *testing.T) {
	f := &fakeBigIP{conns: map[string]int{"10.0.0.1:80": 3}, members: map[string]map[string]any{
		"10.0.0.1:80": {"name": "10.0.0.1:80", "session": "monitor-enabled", "state": "up"},
		"10.0.0.2:80": {"name": "10.0.0.2:80", "session": "monitor-enabled", "state": "down"},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	settings := map[string]string{"management_url": srv.URL, "allow_http": "true", "pool": "shop", "credentials_secret": "bigip", "sync_group": "dg1"}
	if err := validateF5(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := NewF5Pool(ctx, Env{Settings: settings, Secrets: mapSecrets{"bigip": "admin:pw"}})
	if err != nil {
		t.Fatal(err)
	}
	all, err := p.Members(ctx)
	if err != nil || len(all) != 2 || !all[0].InService() || all[1].InService() {
		t.Fatalf("membri: %+v %v", all, err)
	}
	if err := Capacity(all, []string{"10.0.0.1:80"}, 1); err == nil {
		t.Error("svuotato l'unico membro sano")
	}
	if err := p.Drain(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	if c, err := p.Connections(ctx, []string{"10.0.0.1:80"}); err != nil || c["10.0.0.1:80"] != 3 {
		t.Errorf("connessioni: %v %v", c, err)
	}
	if ok, _ := p.Healthy(ctx, []string{"10.0.0.1:80"}); ok["10.0.0.1:80"] {
		t.Error("membro disabilitato in servizio")
	}
	if err := p.Restore(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Healthy(ctx, []string{"10.0.0.1:80"}); !ok["10.0.0.1:80"] {
		t.Error("membro rientrato non in servizio")
	}
	if len(f.syncs) != 2 || f.syncs[0] != "config-sync to-group dg1" {
		t.Errorf("config-sync: %v", f.syncs)
	}
}

// fakeNginxPlus emula l'API di NGINX Plus per un upstream.
type fakeNginxPlus struct {
	mu    sync.Mutex
	peers []map[string]any
}

func (f *fakeNginxPlus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const base = "/api/9/http/upstreams/shop"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		_ = json.NewEncoder(w).Encode(map[string]any{"peers": f.peers})
	case r.Method == http.MethodGet && r.URL.Path == base+"/servers":
		var list []any
		for _, p := range f.peers {
			list = append(list, map[string]any{"id": p["id"], "server": p["server"], "weight": p["weight"], "drain": p["state"] == "draining"})
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, base+"/servers/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, base+"/servers/"))
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, p := range f.peers {
			if int(p["id"].(float64)) == id {
				if in["drain"] == true {
					p["state"] = "draining"
				} else {
					p["state"] = "up"
				}
				if w, ok := in["weight"]; ok {
					p["weight"] = w
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestNginxPlusPool(t *testing.T) {
	f := &fakeNginxPlus{peers: []map[string]any{
		{"id": 0.0, "server": "10.0.0.1:80", "state": "up", "active": 2.0},
		{"id": 1.0, "server": "10.0.0.2:80", "state": "up", "active": 0.0},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	settings := map[string]string{"api_urls": srv.URL + "/api", "allow_http": "true", "upstream": "shop"}
	if err := validateNginxPlus(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := NewNginxPlusPool(ctx, Env{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	all, _ := p.Members(ctx)
	if all[0].State != StateDraining || all[1].State != StateActive {
		t.Errorf("membri: %+v", all)
	}
	if c, _ := p.Connections(ctx, []string{"10.0.0.1:80"}); c["10.0.0.1:80"] != 2 {
		t.Errorf("connessioni: %v", c)
	}
	if err := p.Restore(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Healthy(ctx, []string{"10.0.0.1:80", "10.0.0.9:80"}); !ok["10.0.0.1:80"] || ok["10.0.0.9:80"] {
		t.Errorf("salute: %v", ok)
	}
	if err := p.Drain(ctx, []string{"10.0.0.9:80"}); err == nil || !strings.Contains(err.Error(), "assente") {
		t.Errorf("server assente: %v", err)
	}
}
