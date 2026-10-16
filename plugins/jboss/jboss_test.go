package jboss

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// fakeServer imitates the WildFly HTTP management API: MD5 digest authentication,
// add-content, deployments, atomic composite operations and server groups.
type fakeServer struct {
	t      *testing.T
	domain bool
	groups []string

	mu       sync.Mutex
	content  map[string]bool
	deps     map[string]*fakeDep
	ops      []string
	uploads  int
	failNext string // operation to fail once (e.g. "deploy")
	nonce    int
}

type fakeDep struct {
	runtime, hash string
	enabled       bool
	groups        map[string]bool
}

const (
	testUser = "manent"
	testPass = "s3cret"
	realm    = "ManagementRealm"
)

func newFake(t *testing.T, domain bool, groups ...string) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, domain: domain, groups: groups, content: map[string]bool{}, deps: map[string]*fakeDep{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		f.mu.Lock()
		f.nonce++
		n := f.nonce
		f.mu.Unlock()
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="n%d", opaque="o", algorithm=MD5, qop="auth"`, realm, n))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var res any
	var err error
	if r.URL.Path == "/management/add-content" {
		res, err = f.addContent(r)
	} else {
		var o map[string]any
		if err = json.NewDecoder(r.Body).Decode(&o); err == nil {
			res, err = f.exec(o, true)
		}
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "failed", "failure-description": err.Error(), "rolled-back": true})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "success", "result": res})
}

func (f *fakeServer) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := parseParams(strings.TrimPrefix(h, "Digest "))
	md := func(s string) string { x := md5.Sum([]byte(s)); return hex.EncodeToString(x[:]) }
	ha1 := md(testUser + ":" + realm + ":" + testPass)
	ha2 := md(r.Method + ":" + p["uri"])
	return p["username"] == testUser && p["response"] == md(ha1+":"+p["nonce"]+":"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2)
}

func (f *fakeServer) addContent(r *http.Request) (any, error) {
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(file)
	sum := sha1.Sum(data)
	h := base64.StdEncoding.EncodeToString(sum[:])
	f.content[h] = true
	f.uploads++
	f.ops = append(f.ops, "add-content")
	return map[string]string{"BYTES_VALUE": h}, nil
}

// addr reads the DMR address as a type → name map.
func addr(o map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := o["address"].([]any)
	for _, e := range list {
		for k, v := range e.(map[string]any) {
			out[k] = v.(string)
		}
	}
	return out
}

func (f *fakeServer) exec(o map[string]any, top bool) (any, error) {
	name := o["operation"].(string)
	a := addr(o)
	if top && name != "read-attribute" && name != "read-children-resources" && name != "read-resource" {
		label := name
		if d := a["deployment"]; d != "" {
			label += " " + d
		}
		if g := a["server-group"]; g != "" {
			label += " @" + g
		}
		f.ops = append(f.ops, label)
	}
	if f.failNext == name {
		f.failNext = ""
		return nil, fmt.Errorf("WFLYCTL0080: failed services (%s)", name)
	}
	g, dn := a["server-group"], a["deployment"]
	switch name {
	case "read-attribute":
		switch o["name"] {
		case "launch-type":
			if f.domain {
				return "DOMAIN", nil
			}
			return "STANDALONE", nil
		case "server-state":
			return "running", nil
		case "suspend-state":
			return "SUSPENDED", nil
		}
	case "read-resource":
		for _, x := range f.groups {
			if x == g {
				return map[string]any{}, nil
			}
		}
		return nil, fmt.Errorf("WFLYCTL0216: resource not found %s", g)
	case "read-children-resources":
		out := map[string]any{}
		for n, d := range f.deps {
			switch {
			case g == "":
				m := map[string]any{"runtime-name": d.runtime, "content": []any{map[string]any{"hash": map[string]string{"BYTES_VALUE": d.hash}}}}
				if !f.domain {
					m["enabled"] = d.enabled
				}
				out[n] = m
			default:
				if on, ok := d.groups[g]; ok {
					out[n] = map[string]any{"runtime-name": d.runtime, "enabled": on}
				}
			}
		}
		return out, nil
	case "add":
		if g != "" {
			d := f.deps[dn]
			if d == nil {
				return nil, fmt.Errorf("deployment %s assente dal dominio", dn)
			}
			on, _ := o["enabled"].(bool)
			if on {
				if err := f.unique(d, g); err != nil {
					return nil, err
				}
			}
			d.groups[g] = on
			return nil, nil
		}
		if f.deps[dn] != nil {
			return nil, fmt.Errorf("WFLYCTL0212: duplicate resource %s", dn)
		}
		c := o["content"].([]any)[0].(map[string]any)["hash"].(map[string]any)["BYTES_VALUE"].(string)
		if !f.content[c] {
			return nil, fmt.Errorf("contenuto %s assente", c)
		}
		f.deps[dn] = &fakeDep{runtime: o["runtime-name"].(string), hash: c, groups: map[string]bool{}}
		return nil, nil
	case "deploy", "undeploy":
		d := f.deps[dn]
		if d == nil {
			return nil, fmt.Errorf("deployment %s assente", dn)
		}
		on := name == "deploy"
		if g != "" {
			if _, ok := d.groups[g]; !ok {
				return nil, fmt.Errorf("deployment %s non assegnato a %s", dn, g)
			}
			if on {
				if err := f.unique(d, g); err != nil {
					return nil, err
				}
			}
			d.groups[g] = on
			return nil, nil
		}
		if on {
			if err := f.unique(d, ""); err != nil {
				return nil, err
			}
		}
		d.enabled = on
		return nil, nil
	case "remove":
		d := f.deps[dn]
		if d == nil {
			return nil, fmt.Errorf("deployment %s assente", dn)
		}
		if g != "" {
			delete(d.groups, g)
			return nil, nil
		}
		if d.enabled || len(d.groups) > 0 {
			return nil, fmt.Errorf("deployment %s in uso", dn)
		}
		delete(f.deps, dn)
		return nil, nil
	case "composite":
		// Atomicity: work on a copy and apply it only if all steps succeed.
		saved := f.snapshot()
		for _, st := range o["steps"].([]any) {
			sm := st.(map[string]any)
			label := sm["operation"].(string) + " " + addr(sm)["deployment"]
			if g := addr(sm)["server-group"]; g != "" {
				label += " @" + g
			}
			f.ops = append(f.ops, "  "+label)
			if _, err := f.exec(sm, false); err != nil {
				f.deps = saved
				return nil, err
			}
		}
		return nil, nil
	case "suspend", "resume", "reload", "shutdown", "suspend-servers", "resume-servers", "reload-servers", "restart-servers":
		return nil, nil
	}
	return nil, fmt.Errorf("operazione %s non gestita", name)
}

// unique rejects two active deployments with the same runtime-name, as the server does.
func (f *fakeServer) unique(d *fakeDep, group string) error {
	for n, x := range f.deps {
		if x == d || x.runtime != d.runtime {
			continue
		}
		if group == "" && x.enabled || group != "" && x.groups[group] {
			return fmt.Errorf("WFLYSRV0205: runtime-name %s già in uso da %s", d.runtime, n)
		}
	}
	return nil
}

func (f *fakeServer) snapshot() map[string]*fakeDep {
	out := map[string]*fakeDep{}
	for n, d := range f.deps {
		c := *d
		c.groups = map[string]bool{}
		for k, v := range d.groups {
			c.groups[k] = v
		}
		out[n] = &c
	}
	return out
}

func (f *fakeServer) takeOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.ops
	f.ops = nil
	return out
}

func (f *fakeServer) state() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for n, d := range f.deps {
		s := "off"
		if d.enabled {
			s = "on"
		}
		if f.domain {
			var parts []string
			for _, g := range f.groups {
				if on, ok := d.groups[g]; ok {
					parts = append(parts, g+"="+strconv.FormatBool(on))
				}
			}
			s = strings.Join(parts, ",")
		}
		out[n] = s
	}
	return out
}

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	if v, ok := s[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("segreto %s assente", name)
}

func testPlugin() *Plugin {
	p := New(HTTPConnector{}, []string{"war", "ear"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.poll = time.Millisecond
	return p
}

// request prepares a request for the given version, with a war of different content per version.
func request(t *testing.T, srv *httptest.Server, version string, params map[string]string) *plugin.Request {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shop.war")
	if err := os.WriteFile(path, []byte("war "+version), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	n, _ := strconv.Atoi(port)
	p := map[string]string{ParamUsername: testUser, ParamPasswordSecret: "mgmt", ParamTLS: "false"}
	for k, v := range params {
		p[k] = v
	}
	return &plugin.Request{
		DeploymentID: "d-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "war", Name: "shop", Version: version, LocalPath: path},
		Targets:  []plugin.Target{{Host: host, Port: n}},
		Params:   p, Secrets: secrets{"mgmt": testPass},
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("%s:\n got  %s\n want %s", what, g, w)
	}
}

func TestStandaloneDeployRollbackEPulizia(t *testing.T) {
	f, srv := newFake(t, false)
	p, ctx := testPlugin(), context.Background()

	// First release with suspend and reload: the reload puts the server back in service.
	res, err := p.Deploy(ctx, request(t, srv, "1.0", map[string]string{ParamSuspend: "true", ParamReload: "true", ParamKeepReleases: "2"}))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "operazioni 1.0", f.takeOps(), []string{"add-content", "add shop-1.0.war", "suspend", "composite", "  deploy shop-1.0.war", "reload"})
	eq(t, "stato 1.0", f.state(), map[string]string{"shop-1.0.war": "on"})
	if d := res.Targets[0].Details; d["reloaded"] != "true" || d["previous"] != "" {
		t.Errorf("dettagli: %v", d)
	}

	// Second release with suspend and without reload: explicit resume.
	req2 := request(t, srv, "2.0", map[string]string{ParamSuspend: "true", ParamKeepReleases: "2"})
	if _, err := p.Deploy(ctx, req2); err != nil {
		t.Fatal(err)
	}
	eq(t, "operazioni 2.0", f.takeOps(), []string{"add-content", "add shop-2.0.war", "suspend", "composite", "  undeploy shop-1.0.war", "  deploy shop-2.0.war", "resume"})
	eq(t, "stato 2.0", f.state(), map[string]string{"shop-1.0.war": "off", "shop-2.0.war": "on"})

	rel, err := p.CurrentRelease(ctx, req2)
	if err != nil || rel == nil || rel.Version != "2.0" || rel.Details[DetailDeployment] != "shop-2.0.war" {
		t.Fatalf("release in esercizio: %+v %v", rel, err)
	}

	// Repeating the same deploy (retry or recovery) does not touch the server.
	res, err = p.Deploy(ctx, req2)
	if err != nil || res.Targets[0].Details["unchanged"] != "true" {
		t.Fatalf("deploy ripetuto: %+v %v", res, err)
	}
	eq(t, "operazioni ripetute", f.takeOps(), []string(nil))

	// Rollback to 1.0: reactivation without uploads.
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "1.0", Details: map[string]string{DetailDeployment: "shop-1.0.war"}}); err != nil {
		t.Fatal(err)
	}
	eq(t, "operazioni rollback", f.takeOps(), []string{"suspend", "composite", "  undeploy shop-2.0.war", "  deploy shop-1.0.war", "resume"})
	eq(t, "stato rollback", f.state(), map[string]string{"shop-1.0.war": "on", "shop-2.0.war": "off"})

	// With keep_releases=2 the 3.0 release keeps only the previous one (1.0).
	if _, err := p.Deploy(ctx, request(t, srv, "3.0", map[string]string{ParamKeepReleases: "2"})); err != nil {
		t.Fatal(err)
	}
	eq(t, "stato 3.0", f.state(), map[string]string{"shop-1.0.war": "off", "shop-3.0.war": "on"})
}

func TestStandaloneRestart(t *testing.T) {
	f, srv := newFake(t, false)
	if _, err := testPlugin().Deploy(context.Background(), request(t, srv, "1.0", map[string]string{ParamRestart: "true"})); err != nil {
		t.Fatal(err)
	}
	eq(t, "operazioni", f.takeOps(), []string{"add-content", "add shop-1.0.war", "composite", "  deploy shop-1.0.war", "shutdown"})
}

func TestCambioFallitoRiprendeIlServer(t *testing.T) {
	f, srv := newFake(t, false)
	p, ctx := testPlugin(), context.Background()
	if _, err := p.Deploy(ctx, request(t, srv, "1.0", nil)); err != nil {
		t.Fatal(err)
	}
	f.takeOps()
	f.mu.Lock()
	f.failNext = "deploy"
	f.mu.Unlock()
	res, err := p.Deploy(ctx, request(t, srv, "2.0", map[string]string{ParamSuspend: "true", ParamReload: "true"}))
	if err == nil || !strings.Contains(err.Error(), "WFLYCTL0080") || res.Targets[0].Status != plugin.StatusFailed {
		t.Fatalf("atteso errore del cambio: %v", err)
	}
	// The server rolls the composite back, 1.0 stays active and the server goes back in service
	// without reload.
	eq(t, "operazioni", f.takeOps(), []string{"add-content", "add shop-2.0.war", "suspend", "composite", "  undeploy shop-1.0.war", "  deploy shop-2.0.war", "resume"})
	eq(t, "stato", f.state(), map[string]string{"shop-1.0.war": "on", "shop-2.0.war": "off"})

	// The retry reuses the already uploaded deployment.
	if _, err := p.Deploy(ctx, request(t, srv, "2.0", nil)); err != nil {
		t.Fatal(err)
	}
	eq(t, "stato dopo il retry", f.state(), map[string]string{"shop-1.0.war": "off", "shop-2.0.war": "on"})
	if f.uploads != 2 {
		t.Errorf("caricamenti: %d", f.uploads)
	}
}

func TestContenutoDiversoStessaVersione(t *testing.T) {
	f, srv := newFake(t, false)
	p, ctx := testPlugin(), context.Background()
	if _, err := p.Deploy(ctx, request(t, srv, "1.0", nil)); err != nil {
		t.Fatal(err)
	}
	req := request(t, srv, "1.0", nil)
	if err := os.WriteFile(req.Artifact.LocalPath, []byte("ricostruito"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(ctx, req); err == nil || !strings.Contains(err.Error(), "nuova versione") {
		t.Fatalf("atteso rifiuto: %v", err)
	}
	eq(t, "stato", f.state(), map[string]string{"shop-1.0.war": "on"})
}

func TestDomainMode(t *testing.T) {
	f, srv := newFake(t, true, "main", "other")
	p, ctx := testPlugin(), context.Background()
	params := map[string]string{ParamServerGroups: "main,other", ParamSuspend: "true", ParamReload: "true"}
	if _, err := p.Deploy(ctx, request(t, srv, "1.0", params)); err != nil {
		t.Fatal(err)
	}
	eq(t, "operazioni 1.0", f.takeOps(), []string{"add-content", "add shop-1.0.war", "add shop-1.0.war @main", "add shop-1.0.war @other",
		"suspend-servers @main", "suspend-servers @other", "composite", "  deploy shop-1.0.war @main", "  deploy shop-1.0.war @other",
		"reload-servers @main", "reload-servers @other"})
	req := request(t, srv, "2.0", map[string]string{ParamServerGroups: "main,other", ParamRestart: "true"})
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	eq(t, "stato 2.0", f.state(), map[string]string{"shop-1.0.war": "main=false,other=false", "shop-2.0.war": "main=true,other=true"})
	if ops := f.takeOps(); ops[len(ops)-1] != "restart-servers @other" {
		t.Errorf("restart: %v", ops)
	}
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel.Version != "2.0" {
		t.Fatalf("release: %+v %v", rel, err)
	}

	// Missing or nonexistent server group.
	if _, err := p.Deploy(ctx, request(t, srv, "3.0", nil)); err == nil || !strings.Contains(err.Error(), ParamServerGroups) {
		t.Errorf("atteso errore senza server_groups: %v", err)
	}
	if _, err := p.Deploy(ctx, request(t, srv, "3.0", map[string]string{ParamServerGroups: "nope"})); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("atteso errore sul server group: %v", err)
	}
}

func TestDryRunECredenziali(t *testing.T) {
	f, srv := newFake(t, false)
	p, ctx := testPlugin(), context.Background()
	res, err := p.DryRun(ctx, request(t, srv, "1.0", nil))
	if err != nil || res.Targets[0].Status != plugin.StatusReady || res.Targets[0].Details["mode"] != "standalone" {
		t.Fatalf("dry-run: %+v %v", res, err)
	}
	if ops := f.takeOps(); len(ops) != 0 {
		t.Errorf("il dry-run ha modificato il server: %v", ops)
	}
	req := request(t, srv, "1.0", nil)
	req.Secrets = secrets{"mgmt": "sbagliata"}
	if _, err := p.DryRun(ctx, req); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("attese credenziali rifiutate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	_, srv := newFake(t, false)
	p := testPlugin()
	cases := map[string]func(r *plugin.Request){
		"reload e restart":   func(r *plugin.Request) { r.Params[ParamReload], r.Params[ParamRestart] = "true", "true" },
		"booleano":           func(r *plugin.Request) { r.Params[ParamSuspend] = "si" },
		"password":           func(r *plugin.Request) { delete(r.Params, ParamPasswordSecret) },
		"keep_releases":      func(r *plugin.Request) { r.Params[ParamKeepReleases] = "1" },
		"suspend_timeout":    func(r *plugin.Request) { r.Params[ParamSuspendTimeout] = "x" },
		"runtime_name":       func(r *plugin.Request) { r.Params[ParamRuntimeName] = "../shop.war" },
		"estensione":         func(r *plugin.Request) { r.Artifact.LocalPath, r.Artifact.Type = "/tmp/shop.zip", "application" },
		"versione":           func(r *plugin.Request) { r.Artifact.Version = "1.0/../x" },
		"host":               func(r *plugin.Request) { r.Targets[0].Host = "evil.example.com/x" },
		"runtime_name senza": func(r *plugin.Request) { r.Params[ParamRuntimeName] = "shop" },
	}
	if err := p.Validate(request(t, srv, "1.0", nil)); err != nil {
		t.Fatalf("richiesta valida rifiutata: %v", err)
	}
	for name, mut := range cases {
		req := request(t, srv, "1.0", nil)
		mut(req)
		if err := p.Validate(req); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.9", "1.10", -1}, {"2.0", "1.10", 1}, {"1.0", "1.0", 0}, {"1.0-rc1", "1.0-rc2", -1}, {"1.0", "1.0.1", -1}} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s: %d", c.a, c.b, got)
		}
	}
}
