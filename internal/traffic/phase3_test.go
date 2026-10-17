package traffic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/descriptor"
)

// fakeSSH simulates the NGINX instances: one file per host and the commands used by the pool.
type fakeSSH struct {
	mu      sync.Mutex
	files   map[string]string
	reloads []string
}

func (f *fakeSSH) Dial(_ context.Context, t descriptor.Target, _ string, auth customstep.SSHAuth) (customstep.Conn, error) {
	return &fakeConn{f: f, host: t.Host}, nil
}

type fakeConn struct {
	f    *fakeSSH
	host string
}

func (c *fakeConn) Exec(_ context.Context, cmd string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "cat '"):
		_, _ = io.WriteString(stdout, c.f.files[c.host])
	case strings.HasPrefix(cmd, "cat > '"):
		b, _ := io.ReadAll(stdin)
		c.f.files[c.host] = string(b)
		c.f.reloads = append(c.f.reloads, c.host)
	default:
		return 1, nil
	}
	return 0, nil
}

func (c *fakeConn) Close() error { return nil }

func TestNginxPoolViaSSH(t *testing.T) {
	upstream := "# gestito da manent\nserver 10.0.0.1:80 max_fails=3;\nserver 10.0.0.2:80;\n"
	f := &fakeSSH{files: map[string]string{"lb1": upstream, "lb2": upstream}}
	settings := map[string]string{"lb_hosts": "lb1,lb2", "upstream_file": "/etc/nginx/manent/shop.conf", "ssh_key_secret": "k", "drain_delay": "5s"}
	if err := validateNginx(settings); err != nil {
		t.Fatal(err)
	}
	if err := validateNginx(map[string]string{"upstream_file": "rel; rm -rf /"}); err == nil {
		t.Error("percorso non valido accettato")
	}
	ctx := context.Background()
	p, err := NewNginxPool(ctx, Env{Settings: settings, Dialer: f, Secrets: mapSecrets{"k": "key"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.files["lb2"], "server 10.0.0.1:80 max_fails=3 down;") {
		t.Fatalf("file: %s", f.files["lb2"])
	}
	// Repeated, draining neither rewrites the file nor reloads NGINX.
	if err := p.Drain(ctx, []string{"10.0.0.1:80"}); err != nil || len(f.reloads) != 2 {
		t.Errorf("ricariche: %v %v", f.reloads, err)
	}
	all, _ := p.Members(ctx)
	if len(all) != 2 || all[0].State != StateDisabled || !all[1].InService() {
		t.Errorf("membri: %+v", all)
	}
	if err := p.Restore(ctx, []string{"10.0.0.1:80"}); err != nil {
		t.Fatal(err)
	}
	if f.files["lb1"] != upstream {
		t.Errorf("file dopo il rientro: %q", f.files["lb1"])
	}
	if ok, _ := p.Healthy(ctx, []string{"10.0.0.1:80"}); !ok["10.0.0.1:80"] {
		t.Error("server rientrato non in servizio")
	}
	if err := p.Drain(ctx, []string{"10.0.0.9:80"}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("server assente: %v", err)
	}
}

func TestPesiDeiServer(t *testing.T) {
	members := map[string][]string{"a": {"a1", "a2", "a3"}, "b": {"b1"}}
	w := memberWeights(map[string]int{"a": 75, "b": 25}, members, 256)
	// Every slot gets its share: 3 servers at 25 against 1 at 25.
	if w["a1"] != w["b1"] || w["a1"] == 0 {
		t.Errorf("pesi: %v", w)
	}
	if s := slotShares(w, members); s["a"] != 75 || s["b"] != 25 {
		t.Errorf("quote: %v", s)
	}
	w = memberWeights(map[string]int{"a": 0, "b": 100}, members, 100)
	if w["a1"] != 0 || w["b1"] != 100 {
		t.Errorf("pesi con uno slot a zero: %v", w)
	}
}

func routerEnv(settings map[string]string) Env {
	return Env{Settings: settings, Secrets: mapSecrets{"dp": "admin:s3cret", "bigip": "admin:pw"},
		Targets: []descriptor.Target{
			{Host: "10.0.0.1", Port: 80, Labels: map[string]string{"slot": "a"}},
			{Host: "10.0.0.2", Port: 80, Labels: map[string]string{"slot": "b"}},
		},
		Member: func(t descriptor.Target) string { return t.Host + ":" + strconv.Itoa(t.Port) }}
}

func TestRouterNginxPlus(t *testing.T) {
	f := &fakeNginxPlus{peers: []map[string]any{
		{"id": 0.0, "server": "10.0.0.1:80", "state": "up", "weight": 1.0},
		{"id": 1.0, "server": "10.0.0.2:80", "state": "up", "weight": 1.0},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	r, err := NewNginxPlusRouter(ctx, routerEnv(map[string]string{"api_urls": srv.URL + "/api", "allow_http": "true", "upstream": "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetWeights(ctx, map[string]int{"a": 80, "b": 20}); err != nil {
		t.Fatal(err)
	}
	if w, err := r.Weights(ctx); err != nil || w["a"] != 80 || w["b"] != 20 {
		t.Errorf("quote: %v %v", w, err)
	}
	if err := r.SetWeights(ctx, map[string]int{"a": 0, "b": 100}); err != nil {
		t.Fatal(err)
	}
	if w, _ := r.Weights(ctx); w["a"] != 0 || w["b"] != 100 {
		t.Errorf("quote con lo slot a in drain: %v", w)
	}
}

// fakeDataplaneConfig adds the server configuration API to the Data Plane API.
type fakeDataplaneConfig struct {
	mu      sync.Mutex
	version int
	weights map[string]int
}

func (f *fakeDataplaneConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const base = "/v3/services/haproxy/configuration/backends/shop/servers/"
	switch {
	case r.URL.Path == "/v3/services/haproxy/configuration/version":
		_ = json.NewEncoder(w).Encode(f.version)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base):
		name := strings.TrimPrefix(r.URL.Path, base)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "weight": f.weights[name]})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, base):
		if r.URL.Query().Get("version") != strconv.Itoa(f.version) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.weights[strings.TrimPrefix(r.URL.Path, base)] = int(in["weight"].(float64))
		f.version++
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRouterHAProxy(t *testing.T) {
	f := &fakeDataplaneConfig{version: 7, weights: map[string]int{"10.0.0.1:80": 100, "10.0.0.2:80": 0}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	r, err := NewHAProxyRouter(ctx, routerEnv(map[string]string{"dataplane_urls": srv.URL, "allow_http": "true", "backend": "shop", "credentials_secret": "dp"}))
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := r.Weights(ctx); w["a"] != 100 || w["b"] != 0 {
		t.Errorf("quote iniziali: %v", w)
	}
	if err := r.SetWeights(ctx, map[string]int{"a": 50, "b": 50}); err != nil {
		t.Fatal(err)
	}
	if f.weights["10.0.0.1:80"] != 256 || f.weights["10.0.0.2:80"] != 256 || f.version != 9 {
		t.Errorf("pesi: %v versione %d", f.weights, f.version)
	}
}

func TestRouterF5(t *testing.T) {
	f := &fakeBigIP{conns: map[string]int{}, members: map[string]map[string]any{
		"10.0.0.1:80": {"name": "10.0.0.1:80", "session": "monitor-enabled", "state": "up", "ratio": 1.0},
		"10.0.0.2:80": {"name": "10.0.0.2:80", "session": "user-disabled", "state": "up", "ratio": 1.0},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading a single member and ratio, besides the pool calls.
		const base = "/mgmt/tm/ltm/pool/~Common~shop/members/~Common~"
		if strings.HasPrefix(r.URL.Path, base) && !strings.HasSuffix(r.URL.Path, "/stats") {
			f.mu.Lock()
			m := f.members[strings.TrimPrefix(r.URL.Path, base)]
			if r.Method == http.MethodPatch {
				var in map[string]any
				_ = json.NewDecoder(r.Body).Decode(&in)
				for k, v := range in {
					m[k] = v
				}
			}
			_ = json.NewEncoder(w).Encode(m)
			f.mu.Unlock()
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	r, err := NewF5Router(ctx, routerEnv(map[string]string{"management_url": srv.URL, "allow_http": "true", "pool": "shop", "credentials_secret": "bigip"}))
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := r.Weights(ctx); w["a"] != 100 || w["b"] != 0 {
		t.Errorf("quote iniziali: %v", w)
	}
	if err := r.SetWeights(ctx, map[string]int{"a": 90, "b": 10}); err != nil {
		t.Fatal(err)
	}
	if w, _ := r.Weights(ctx); w["a"] != 90 || w["b"] != 10 {
		t.Errorf("quote: %v (membri %v)", w, f.members)
	}
}
