package kubernetes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// fakeCluster is a minimal API server: discovery, server-side apply, get, list and delete.
// A Deployment with an image containing "broken" never completes its rollout.
type fakeCluster struct {
	mu      sync.Mutex
	objects map[string]Object // path → object
	applies int
	dryRuns int
	// rv is the global resourceVersion, incremented at every write.
	rv int
	// deployed, shared by several clusters, counts the Deployments applied: until it reaches
	// hold no rollout appears observed.
	deployed *atomic.Int32
	hold     int32
}

var discovery = map[string]string{
	"/api/v1": `{"resources":[
		{"name":"configmaps","kind":"ConfigMap","namespaced":true},
		{"name":"services","kind":"Service","namespaced":true},
		{"name":"namespaces","kind":"Namespace","namespaced":false},
		{"name":"namespaces/status","kind":"Namespace","namespaced":false}]}`,
	"/apis/apps/v1":                      `{"resources":[{"name":"deployments","kind":"Deployment","namespaced":true}]}`,
	"/apis/gateway.networking.k8s.io/v1": `{"resources":[{"name":"httproutes","kind":"HTTPRoute","namespaced":true}]}`,
	"/apis/networking.istio.io/v1":       `{"resources":[{"name":"virtualservices","kind":"VirtualService","namespaced":true}]}`,
	"/apis": `{"groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1"}]},
		{"name":"monitoring.coreos.com","versions":[{"groupVersion":"monitoring.coreos.com/v1"}]}]}`,
}

func (c *fakeCluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer t0k" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"Unauthorized"}`))
		return
	}
	if r.URL.Path == "/version" {
		_, _ = w.Write([]byte(`{"gitVersion":"v1.31.2"}`))
		return
	}
	if d, ok := discovery[r.URL.Path]; ok {
		_, _ = w.Write([]byte(d))
		return
	}
	switch r.Method {
	case http.MethodPatch:
		if r.Header.Get("Content-Type") == "application/merge-patch+json" {
			prev, ok := c.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"not found","reason":"NotFound"}`))
				return
			}
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if md, _ := patch["metadata"].(map[string]any); md != nil && md["resourceVersion"] != nil && md["resourceVersion"] != prev.str("metadata", "resourceVersion") {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"the object has been modified","reason":"Conflict"}`))
				return
			}
			mergePatch(prev, patch)
			if prev.Kind() == "Deployment" {
				// The fake controller immediately brings replicas to the requested value.
				gen := num(prev, "metadata", "generation") + 1
				n := float64(num(prev, "spec", "replicas"))
				prev.metadata()["generation"] = float64(gen)
				prev["status"] = map[string]any{"observedGeneration": float64(gen), "replicas": n, "updatedReplicas": n, "availableReplicas": n}
			}
			c.rv++
			prev.metadata()["resourceVersion"] = strconv.Itoa(c.rv)
			_ = json.NewEncoder(w).Encode(prev)
			return
		}
		if r.Header.Get("Content-Type") != "application/apply-patch+yaml" || r.URL.Query().Get("fieldManager") != FieldManager {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var o Object
		_ = json.NewDecoder(r.Body).Decode(&o)
		if r.URL.Query().Get("dryRun") == "All" {
			c.dryRuns++
			_ = json.NewEncoder(w).Encode(o)
			return
		}
		c.applies++
		gen := 1
		if prev, ok := c.objects[r.URL.Path]; ok {
			gen = num(prev, "metadata", "generation") + 1
			o.metadata()["creationTimestamp"] = prev.str("metadata", "creationTimestamp")
		} else {
			o.metadata()["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		o.metadata()["generation"] = float64(gen)
		c.rv++
		o.metadata()["resourceVersion"] = strconv.Itoa(c.rv)
		if o.Kind() == "Deployment" {
			if c.deployed != nil {
				c.deployed.Add(1)
			}
			st := map[string]any{"observedGeneration": float64(gen), "replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1)}
			for _, img := range images(o) {
				if strings.Contains(img, "broken") {
					st = map[string]any{"observedGeneration": float64(gen), "conditions": []any{map[string]any{
						"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded", "message": "ImagePullBackOff"}}}
				}
			}
			o["status"] = st
		}
		// JSON round trip, as in a real cluster.
		b, _ := json.Marshal(o)
		var stored Object
		_ = json.Unmarshal(b, &stored)
		c.objects[r.URL.Path] = stored
		_, _ = w.Write(b)
	case http.MethodGet:
		if o, ok := c.objects[r.URL.Path]; ok {
			if o.Kind() == "Deployment" && c.deployed != nil && c.deployed.Load() < c.hold {
				o = Object{"apiVersion": o["apiVersion"], "kind": o["kind"], "metadata": o["metadata"], "status": map[string]any{"observedGeneration": float64(0)}}
			}
			_ = json.NewEncoder(w).Encode(o)
			return
		}
		if sel := r.URL.Query().Get("labelSelector"); sel != "" || strings.HasSuffix(r.URL.Path, "/configmaps") {
			var items []Object
			for p, o := range c.objects {
				if !strings.HasPrefix(p, r.URL.Path+"/") || !matches(o, sel) {
					continue
				}
				items = append(items, o)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"not found","reason":"NotFound"}`))
	case http.MethodDelete:
		if _, ok := c.objects[r.URL.Path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(c.objects, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}
}

// mergePatch applies a JSON merge patch (RFC 7386).
func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			cur, ok := dst[k].(map[string]any)
			if !ok {
				cur = map[string]any{}
				dst[k] = cur
			}
			mergePatch(cur, pv)
		default:
			dst[k] = pv
		}
	}
}

func matches(o Object, sel string) bool {
	labels, _ := o.metadata()["labels"].(map[string]any)
	for _, pair := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(pair, "=")
		if k != "" && labels[k] != v {
			return false
		}
	}
	return true
}

func (c *fakeCluster) get(path string) Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.objects[path]
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", errors.New("segreto non trovato")
}

const manifestV1 = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shop
spec:
  replicas: ${REPLICAS}
  template:
    spec:
      containers:
        - name: web
          image: registry/shop:${MANENT_VERSION}
---
apiVersion: v1
kind: Service
metadata:
  name: shop
spec:
  ports: [{port: 80}]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-config
data:
  script: "echo ${HOME}"
`

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shop.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func setup(t *testing.T) (*Plugin, *fakeCluster, string) {
	t.Helper()
	fc := &fakeCluster{objects: map[string]Object{}}
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)
	p := New(HTTPConnector{}, []string{"kubernetes"}, slog.New(slog.DiscardHandler))
	p.poll = 10 * time.Millisecond
	return p, fc, srv.URL
}

func request(server, version, manifest string, t *testing.T) *plugin.Request {
	return &plugin.Request{
		DeploymentID: "d-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "kubernetes", Name: "shop", Version: version, SHA256: "abc", LocalPath: writeManifest(t, manifest)},
		Targets:  []plugin.Target{{Host: server}},
		Params:   map[string]string{ParamNamespace: "shop", ParamTokenSecret: "tok", "var.REPLICAS": "1", ParamWaitTimeout: "2s"},
		Secrets:  mapSecrets{"tok": "t0k\n"},
	}
}

const (
	deployPath  = "/apis/apps/v1/namespaces/shop/deployments/shop"
	servicePath = "/api/v1/namespaces/shop/services/shop"
	extraPath   = "/api/v1/namespaces/shop/configmaps/shop-extra"
	pointerPath = "/api/v1/namespaces/shop/configmaps/manent-shop-prod"
)

func image(o Object) string {
	if imgs := images(o); len(imgs) > 0 {
		return imgs[0]
	}
	return ""
}

func TestDeployERollback(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)

	v1 := request(server, "1.0.0", manifestV1, t)
	if err := p.Validate(v1); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(ctx, v1)
	if err != nil {
		t.Fatal(err)
	}
	d := fc.get(deployPath)
	if image(d) != "registry/shop:1.0.0" || num(d, "spec", "replicas") != 1 {
		t.Fatalf("deployment inatteso: %v", d)
	}
	if fc.get("/api/v1/namespaces/shop/configmaps/shop-config").str("data", "script") != "echo ${HOME}" {
		t.Error("le variabili non note devono restare invariate")
	}
	if d.str("metadata", "labels", LabelManagedBy) != "manent" || d.str("metadata", "annotations", AnnotationVersion) != "1.0.0" {
		t.Errorf("etichette di manent mancanti: %v", d["metadata"])
	}
	if fc.get(pointerPath).str("data", "version") != "1.0.0" || res.Targets[0].Details["objects"] != "3" {
		t.Errorf("puntatore o esito inatteso: %v %+v", fc.get(pointerPath), res)
	}
	rel, err := p.CurrentRelease(ctx, v1)
	if err != nil || rel == nil || rel.Version != "1.0.0" {
		t.Fatalf("release in esercizio: %+v %v", rel, err)
	}

	// 2.0.0 adds a ConfigMap and has an image that does not start: the rollout fails and the
	// pointer stays on 1.0.0.
	broken := strings.Replace(manifestV1, "registry/shop:${MANENT_VERSION}", "registry/shop:broken", 1) +
		"---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop-extra\n"
	v2 := request(server, "2.0.0", broken, t)
	v2.Params[ParamPrune] = "true"
	if _, err := p.Deploy(ctx, v2); err == nil || !strings.Contains(err.Error(), "rollout stuck") || !errors.Is(err, plugin.ErrPermanent) {
		t.Fatalf("atteso rollout bloccato: %v", err)
	}
	if fc.get(pointerPath).str("data", "version") != "1.0.0" || fc.get(extraPath) == nil {
		t.Fatal("il puntatore non deve cambiare e la ConfigMap della 2.0.0 è stata applicata")
	}
	if _, err := p.Rollback(ctx, v2, *rel); err != nil {
		t.Fatal(err)
	}
	if image(fc.get(deployPath)) != "registry/shop:1.0.0" || fc.get(extraPath) != nil || fc.get(servicePath) == nil {
		t.Errorf("rollback incompleto: deployment %s, extra %v, service %v", image(fc.get(deployPath)), fc.get(extraPath), fc.get(servicePath))
	}

	// 3.0.0 no longer has the Service: with prune it is deleted.
	v3 := request(server, "3.0.0", strings.Split(manifestV1, "---")[0]+"---"+strings.Split(manifestV1, "---")[2], t)
	v3.Params[ParamPrune] = "true"
	res, err = p.Deploy(ctx, v3)
	if err != nil {
		t.Fatal(err)
	}
	if fc.get(servicePath) != nil || res.Targets[0].Details["pruned"] != "Service/shop/shop" {
		t.Errorf("prune: %+v", res.Targets[0].Details)
	}
}

func TestStoricoEDryRun(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0"} {
		req := request(server, v, manifestV1, t)
		req.Params[ParamKeepReleases] = "2"
		if _, err := p.Deploy(ctx, req); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var history []string
	for path := range fc.objects {
		if strings.HasPrefix(path, "/api/v1/namespaces/shop/configmaps/manent-shop-prod-") {
			history = append(history, path)
		}
	}
	if len(history) != 2 {
		t.Errorf("storico oltre keep_releases: %v", history)
	}

	before := fc.applies
	req := request(server, "1.4.0", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: nuovo\n---\n"+
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: nuovo\n---\n"+manifestV1, t)
	res, err := p.DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Targets[0].Details
	if fc.applies != before || d["cluster_version"] != "v1.31.2" || d["objects_checked"] != "4" || d["objects_skipped"] != "1" || d["current"] != "1.3.0" {
		t.Errorf("dry-run inatteso: %+v (apply reali %d)", d, fc.applies-before)
	}

	// Wrong credentials: a clear error already in dry-run.
	req.Secrets = mapSecrets{"tok": "sbagliato"}
	if _, err := p.DryRun(ctx, req); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("atteso errore di autenticazione: %v", err)
	}
}

func TestValidazioneEManifest(t *testing.T) {
	p, _, server := setup(t)
	bad := request(server, "", manifestV1, t)
	bad.Params[ParamNamespace], bad.Params[ParamCertSecret], bad.Params[ParamWaitTimeout], bad.Params[ParamKeepReleases] = "Shop_", "cert", "mai", "1"
	bad.Artifact.LocalPath = "/tmp/shop" + plugin.ImageRefExt
	err := p.Validate(bad)
	for _, want := range []string{"artifact.version", "namespace", "token_secret", "wait_timeout", "keep_releases", "image"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validazione: manca %q in %v", want, err)
		}
	}

	// Images not pinned to the digest with require_image_digest.
	req := request(server, "1.0.0", manifestV1, t)
	req.Params[ParamRequireDigest] = "true"
	if _, err := p.Deploy(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not pinned to the digest") {
		t.Errorf("atteso errore sul digest: %v", err)
	}

	// tar.gz archive: files are read in name order, the others ignored.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{
		{"b/service.yaml", "apiVersion: v1\nkind: Service\nmetadata: {name: b}\n"},
		{"a/config.yml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n"},
		{"README.md", "# no"},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		_, _ = io.WriteString(tw, f.body)
	}
	_ = tw.Close()
	_ = zw.Close()
	archive := filepath.Join(t.TempDir(), "m.tar.gz")
	_ = os.WriteFile(archive, buf.Bytes(), 0o644)
	raw, err := loadManifests(archive)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := parseObjects(raw)
	if err != nil || len(objs) != 2 || objs[0].Name() != "a" {
		t.Errorf("archivio: %v %v", objs, err)
	}
	if _, err := parseObjects([]byte("kind: ConfigMap\n")); err == nil {
		t.Error("atteso errore per un oggetto senza apiVersion")
	}
	for _, n := range []string{"manent-shop-prod-1.2.0+build", strings.Repeat("a", 80)} {
		if got := dnsName(n); !dnsLabelRe.MatchString(got) {
			t.Errorf("dnsName(%q) = %q non valido", n, got)
		}
	}
}

// Every release keeps its own history: two artifacts of the same application, and the same
// artifact of a tenant, neither share the pointer nor prune each other's releases.
func TestStoricoPerRelease(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)
	api := strings.NewReplacer("name: shop\n", "name: shop-api\n", "app: shop", "app: shop-api").Replace(manifestV1)
	deploy := func(artifact, tenant, version, manifest string) {
		t.Helper()
		req := request(server, version, manifest, t)
		req.Artifact.Name, req.Tenant = artifact, tenant
		req.Params[ParamKeepReleases] = "2"
		if _, err := p.Deploy(ctx, req); err != nil {
			t.Fatalf("%s/%s %s: %v", tenant, artifact, version, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		deploy("shop", "", v, manifestV1)
	}
	for _, v := range []string{"2.0.0", "2.1.0", "2.2.0"} {
		deploy("shop-api", "untenanted", v, api)
	}
	apiNames := historyNames(&plugin.Request{Tenant: "untenanted", Application: "shop", Environment: "prod", Artifact: plugin.Artifact{Name: "shop-api"}})
	if apiNames.pointer == "manent-shop-prod" || !strings.HasPrefix(apiNames.pointer, "manent-untenanted-shop-shop-api-prod-") {
		t.Fatalf("nome dello storico dell'artefatto: %s", apiNames.pointer)
	}
	count := func(prefix string) int {
		n := 0
		for path, o := range fc.objects {
			if strings.HasPrefix(path, "/api/v1/namespaces/shop/configmaps/"+prefix+"-") && o.str("metadata", "labels", LabelRole) == roleRelease {
				n++
			}
		}
		return n
	}
	if got := fc.get(pointerPath).str("data", "version"); got != "1.2.0" {
		t.Errorf("il puntatore dello storico con il nome di sempre: %s", got)
	}
	if got := fc.get("/api/v1/namespaces/shop/configmaps/"+apiNames.pointer).str("data", "version"); got != "2.2.0" {
		t.Errorf("il puntatore dell'artefatto shop-api: %s", got)
	}
	// keep_releases 2 for each release: the pruning of shop-api did not touch shop.
	if a, b := count("manent-shop-prod"), count(apiNames.prefix); a != 2 || b != 2 {
		t.Errorf("storico per release: shop %d, shop-api %d", a, b)
	}
	// The same artifact of a tenant is a third release.
	deploy("shop", "payments", "3.0.0", manifestV1)
	pay := historyNames(&plugin.Request{Tenant: "payments", Application: "shop", Environment: "prod", Artifact: plugin.Artifact{Name: "shop"}})
	if pay.pointer == "manent-shop-prod" || fc.get("/api/v1/namespaces/shop/configmaps/"+pay.pointer).str("data", "version") != "3.0.0" {
		t.Errorf("storico del tenant: %s", pay.pointer)
	}
	if count("manent-shop-prod") != 2 || fc.get(pointerPath).str("data", "version") != "1.2.0" {
		t.Error("lo storico untenanted non deve cambiare")
	}
	if d := fc.get(deployPath); d.str("metadata", "labels", LabelTenant) != "payments" || d.str("metadata", "labels", LabelArtifact) != "shop" {
		t.Errorf("etichette della release sugli oggetti: %v", d.metadata()["labels"])
	}
}

func TestDeployInParallelo(t *testing.T) {
	p, _, _ := setup(t)
	if !plugin.SupportsParallel(p) {
		t.Fatal("ParallelTargets")
	}
	// No rollout is observed before the Deployment is applied on all three clusters: one
	// cluster at a time the first wait would expire.
	var deployed atomic.Int32
	var targets []plugin.Target
	var clusters []*fakeCluster
	for range 3 {
		fc := &fakeCluster{objects: map[string]Object{}, deployed: &deployed, hold: 3}
		srv := httptest.NewServer(fc)
		t.Cleanup(srv.Close)
		targets, clusters = append(targets, plugin.Target{Host: srv.URL}), append(clusters, fc)
	}
	req := request(targets[0].Host, "1.0.0", manifestV1, t)
	req.Targets, req.Parallelism = targets, 3
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Targets {
		if r.Host != targets[i].Host || r.Status != plugin.StatusDeployed || clusters[i].get(pointerPath).str("data", "version") != "1.0.0" {
			t.Errorf("cluster %d: %+v", i, r)
		}
	}

	req2 := request(targets[0].Host, "2.0.0", manifestV1, t)
	req2.Targets, req2.Params[ParamWaitTimeout] = targets, "300ms"
	for _, fc := range clusters {
		fc.mu.Lock()
		fc.hold = deployed.Load() + 3
		fc.mu.Unlock()
	}
	if _, err := p.Deploy(context.Background(), req2); err == nil || !strings.Contains(err.Error(), "not completed") {
		t.Fatalf("in sequenza la prima attesa deve scadere: %v", err)
	}
}
