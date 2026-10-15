package cloudrun

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
)

const svcName = "projects/acme-prod/locations/europe-west1/services/shop"

// fakeRun emula l'Admin API v2 di Cloud Run e l'endpoint dei token OAuth. Un aggiornamento
// del template crea una revisione; con un'immagine che contiene "broken" la revisione non
// diventa pronta e l'operazione fallisce, come in Cloud Run.
type fakeRun struct {
	mu        sync.Mutex
	t         *testing.T
	svc       Object
	revisions []Object
	patches   int
	validates int
	clock     time.Time
	ops       map[string]*Operation
}

func newFakeRun(t *testing.T) *fakeRun {
	f := &fakeRun{t: t, clock: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ops: map[string]*Operation{}}
	f.svc = Object{
		"name": svcName, "etag": "e1", "uri": "https://shop-abc.a.run.app",
		"template": Object{
			"revision":       "shop-iac",
			"serviceAccount": "shop@acme-prod.iam.gserviceaccount.com",
			"containers": []any{
				Object{"name": "shop", "image": "europe-docker.pkg.dev/acme/shop/shop:old", "env": []any{Object{"name": "MODE", "value": "prod"}}},
				Object{"name": "proxy", "image": "envoyproxy/envoy:v1.31"},
			},
		},
		"traffic": []any{Object{"type": trafficLatest, "percent": 100}},
	}
	f.addRevision(nil, false)
	return f
}

// addRevision crea una revisione dal template corrente, con il nome indicato o generato.
func (f *fakeRun) addRevision(ann map[string]any, broken bool, fixed ...string) string {
	f.clock = f.clock.Add(time.Minute)
	name := fmt.Sprintf("%s/revisions/shop-%05d", svcName, len(f.revisions)+1)
	if len(fixed) > 0 && fixed[0] != "" {
		name = svcName + "/revisions/" + fixed[0]
	}
	rev := Object{"name": name, "createTime": f.clock.Format(time.RFC3339), "annotations": ann}
	f.revisions = append(f.revisions, rev)
	f.svc["latestCreatedRevision"] = name
	if !broken {
		f.svc["latestReadyRevision"] = name
	}
	f.updateStatus()
	return name
}

func (f *fakeRun) updateStatus() {
	var sts []any
	for _, raw := range f.svc["traffic"].([]any) {
		tr := raw.(map[string]any)
		st := Object{"type": tr["type"], "percent": tr["percent"]}
		if tr["type"] == trafficRevision {
			st["revision"] = tr["revision"]
		}
		if tag, _ := tr["tag"].(string); tag != "" {
			st["tag"], st["uri"] = tag, "https://"+tag+"---shop-abc.a.run.app"
		}
		sts = append(sts, st)
	}
	f.svc["trafficStatuses"] = sts
}

func (f *fakeRun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.URL.Path == "/token" {
		if err := r.ParseForm(); err != nil || r.Form.Get("assertion") == "" {
			reply(http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		reply(http.StatusOK, map[string]any{"access_token": "t0k", "token_type": "Bearer", "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer t0k" {
		reply(http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401, "message": "unauthenticated"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	notFound := func() {
		reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": path + " not found", "status": "NOT_FOUND"}})
	}
	switch {
	case path == svcName && r.Method == http.MethodGet:
		reply(http.StatusOK, f.svc)
	case path == svcName && r.Method == http.MethodPatch:
		var in Object
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["etag"] != f.svc["etag"] {
			reply(http.StatusConflict, map[string]any{"error": map[string]any{"code": 409, "message": "etag mismatch"}})
			return
		}
		if r.URL.Query().Get("validateOnly") == "true" {
			f.validates++
			reply(http.StatusOK, Operation{Name: "projects/acme-prod/locations/europe-west1/operations/validate", Done: true})
			return
		}
		f.patches++
		// Un template cambiato con il nome di una revisione esistente è rifiutato da Cloud Run.
		fixed, _ := get(in, "template", "revision").(string)
		changed := fmt.Sprint(in["template"]) != fmt.Sprint(f.svc["template"])
		if changed && fixed != "" && slices.ContainsFunc(f.revisions, func(r Object) bool { return short(str(r, "name")) == fixed }) {
			f.t.Error("il template inviato ha il nome di una revisione esistente")
		}
		f.svc["template"], f.svc["traffic"] = in["template"], in["traffic"]
		f.svc["etag"] = fmt.Sprintf("e%d", f.patches+1)
		op := &Operation{Name: fmt.Sprintf("projects/acme-prod/locations/europe-west1/operations/op%d", f.patches)}
		f.ops[op.Name] = op
		if changed {
			image := str(containers(in)[0], "image")
			ann, _ := get(in, "template", "annotations").(map[string]any)
			broken := strings.Contains(image, "broken")
			rev := f.addRevision(ann, broken, fixed)
			if broken {
				f.ops[op.Name] = &Operation{Name: op.Name, Done: true, Error: &OpFailure{Code: 9, Message: "Revision '" + short(rev) + "' is not ready and cannot serve traffic."}}
			}
		} else {
			f.updateStatus()
		}
		reply(http.StatusOK, Operation{Name: op.Name})
	case strings.HasPrefix(path, "projects/acme-prod/locations/europe-west1/operations/"):
		op, ok := f.ops[path]
		if !ok {
			notFound()
			return
		}
		op.Done = true
		reply(http.StatusOK, op)
	case path == svcName+"/revisions":
		reply(http.StatusOK, map[string]any{"revisions": f.revisions})
	case strings.HasPrefix(path, svcName+"/revisions/"):
		for _, rev := range f.revisions {
			if rev["name"] == path {
				reply(http.StatusOK, rev)
				return
			}
		}
		notFound()
	default:
		notFound()
	}
}

func (f *fakeRun) traffic() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	rev, _ := serving(f.svc)
	return rev
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("segreto %q assente", name)
	}
	return v, nil
}

// serviceAccountKey genera la chiave JSON di un service account il cui endpoint dei token è
// quello del server di prova.
func serviceAccountKey(t *testing.T, tokenURL string) string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme-prod", "private_key_id": "k1",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "manent@acme-prod.iam.gserviceaccount.com", "token_uri": tokenURL,
	})
	return string(b)
}

func setup(t *testing.T) (*Plugin, *fakeRun, string) {
	f := newFakeRun(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(&HTTPConnector{Endpoint: srv.URL}, []string{Name}, slog.New(slog.DiscardHandler))
	p.poll = 0
	return p, f, serviceAccountKey(t, srv.URL+"/token")
}

const digest = "sha256:" + "ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56"

func request(t *testing.T, key, version, image string) *plugin.Request {
	path := filepath.Join(t.TempDir(), "shop"+plugin.ImageRefExt)
	if err := os.WriteFile(path, []byte(image), 0o600); err != nil {
		t.Fatal(err)
	}
	return &plugin.Request{
		DeploymentID: "shop-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: Name, Name: "shop", Version: version, LocalPath: path},
		Targets:  []plugin.Target{{Host: "europe-west1"}},
		Params:   map[string]string{ParamProject: "acme-prod", ParamCredentialsSecret: "gcp-key"},
		Secrets:  mapSecrets{"gcp-key": key},
	}
}

func TestDeployERollback(t *testing.T) {
	p, f, key := setup(t)
	ctx := context.Background()
	img1 := "europe-docker.pkg.dev/acme/shop/shop@" + digest
	req := request(t, key, "1.0.0", img1)
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	// La revisione creata fuori da manent non è una release.
	if rel, err := p.CurrentRelease(ctx, req); err != nil || rel != nil {
		t.Fatalf("release prima del primo deploy: %+v, %v", rel, err)
	}
	res, err := p.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Targets[0].Details; d[DetailRevision] != "shop-00002" || d["url"] != "https://shop-abc.a.run.app" {
		t.Fatalf("dettagli: %v", d)
	}
	cs := containers(f.svc)
	if str(cs[0], "image") != img1 || str(cs[1], "image") != "envoyproxy/envoy:v1.31" || get(cs[0], "env") == nil {
		t.Errorf("container: %v", cs)
	}
	if _, err := p.Deploy(ctx, req); err != nil || f.patches != 1 {
		t.Fatalf("deploy ripetuto: %v, %d aggiornamenti", err, f.patches)
	}
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" || rel.Details[DetailRevision+"@europe-west1"] != "shop-00002" {
		t.Fatalf("release in esercizio: %+v, %v", rel, err)
	}

	// Una revisione che non si avvia fa fallire il deploy; il traffico resta sulla precedente.
	broken := request(t, key, "2.0.0", "europe-docker.pkg.dev/acme/shop/shop-broken@"+digest)
	if _, err := p.Deploy(ctx, broken); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("deploy fallito atteso: %v", err)
	}
	if f.traffic() != "shop-00002" {
		t.Fatalf("traffico su %s", f.traffic())
	}

	req2 := request(t, key, "2.0.0", "europe-docker.pkg.dev/acme/shop/shop-v2@"+digest)
	res, err = p.Deploy(ctx, req2)
	if err != nil || f.traffic() != "shop-00004" || res.Targets[0].Details["previous"] != "1.0.0" {
		t.Fatalf("seconda release: %v, traffico %s, %v", err, f.traffic(), res.Targets[0].Details)
	}
	if _, err := p.Rollback(ctx, req2, *rel); err != nil || f.traffic() != "shop-00002" {
		t.Fatalf("rollback: %v, traffico %s", err, f.traffic())
	}
	if cur, err := p.CurrentRelease(ctx, req2); err != nil || cur == nil || cur.Version != "1.0.0" {
		t.Errorf("dopo il rollback: %+v, %v", cur, err)
	}
	// Senza dettagli si sceglie la revisione più recente con la versione (non quella fallita).
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "2.0.0"}); err != nil || f.traffic() != "shop-00004" {
		t.Fatalf("rollback senza dettagli: %v, traffico %s", err, f.traffic())
	}
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "0.1.0"}); err == nil || !strings.Contains(err.Error(), "nessuna revisione") {
		t.Errorf("versione assente: %v", err)
	}
	// Dopo un rollback il deploy successivo riporta il traffico sull'ultima revisione.
	req3 := request(t, key, "3.0.0", "europe-docker.pkg.dev/acme/shop/shop-v3@"+digest)
	if _, err := p.Deploy(ctx, req3); err != nil || f.traffic() != "shop-00005" || !latestOnly(f.svc) {
		t.Fatalf("deploy dopo il rollback: %v, traffico %s", err, f.traffic())
	}

	// Con il traffico diviso (rilascio per percentuale in corso) vale la revisione con la quota
	// maggiore; a parità la release in esercizio non è determinabile.
	f.svc["trafficStatuses"] = []any{Object{"type": trafficRevision, "revision": "shop-00004", "percent": 90.0}, Object{"type": trafficRevision, "revision": "shop-00005", "percent": 10.0}}
	if rel, err := p.CurrentRelease(ctx, req3); err != nil || rel.Details[DetailRevision+"@europe-west1"] != "shop-00004" {
		t.Errorf("traffico diviso: %v %+v", err, rel)
	}
	f.svc["trafficStatuses"] = []any{Object{"type": trafficRevision, "revision": "shop-00004", "percent": 50.0}, Object{"type": trafficRevision, "revision": "shop-00005", "percent": 50.0}}
	if _, err := p.CurrentRelease(ctx, req3); err == nil || !strings.Contains(err.Error(), "parti uguali") {
		t.Errorf("traffico diviso a metà: %v", err)
	}
}

func TestDryRunEValidazione(t *testing.T) {
	p, f, key := setup(t)
	ctx := context.Background()
	req := request(t, key, "1.0.0", "europe-docker.pkg.dev/acme/shop/shop@"+digest)
	res, err := p.DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Targets[0].Details; res.Targets[0].Status != plugin.StatusReady || d["container"] != "shop" {
		t.Errorf("dry-run: %+v", res.Targets[0])
	}
	if f.validates != 1 || f.patches != 0 || len(f.revisions) != 1 {
		t.Error("il dry-run ha modificato il servizio")
	}
	wrong := request(t, key, "1.0.0", "europe-docker.pkg.dev/acme/shop/shop@"+digest)
	wrong.Application, wrong.Params[ParamService] = "web", "shop"
	if _, err := p.DryRun(ctx, wrong); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Errorf("container assente: %v", err)
	}
	missing := request(t, key, "1.0.0", "europe-docker.pkg.dev/acme/shop/shop@"+digest)
	missing.Params[ParamService] = "cart"
	if _, err := p.DryRun(ctx, missing); err == nil || !strings.Contains(err.Error(), "assente") {
		t.Errorf("servizio assente: %v", err)
	}
	badKey := request(t, `{"type":"external_account"}`, "1.0.0", "x")
	if _, err := p.DryRun(ctx, badKey); err == nil || !strings.Contains(err.Error(), "gcp-key") {
		t.Errorf("credenziali non di service account: %v", err)
	}

	bad := request(t, key, "", "x")
	bad.Artifact.LocalPath = "/tmp/app.zip"
	bad.Application = "Shop_API"
	bad.Targets = []plugin.Target{{Host: "eu-west-1a"}}
	bad.Params[ParamProject] = ""
	bad.Params[ParamWaitTimeout] = "-1s"
	err = p.Validate(bad)
	for _, want := range []string{"artifact.version", "immagine di un registry", ParamProject, "Shop_API", "eu-west-1a", ParamWaitTimeout} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validazione: manca %q in %v", want, err)
		}
	}
}

func TestApplicationDefaultCredentials(t *testing.T) {
	p, _, key := setup(t)
	file := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(file, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	req := request(t, "", "1.0.0", "europe-docker.pkg.dev/acme/shop/shop@"+digest)
	delete(req.Params, ParamCredentialsSecret)
	if _, err := p.DryRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}
