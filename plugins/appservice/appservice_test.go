package appservice

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/azurefake"
	"github.com/manent-deployer/manent/pkg/plugin"
)

const sub = "11111111-2222-3333-4444-555555555555"

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", errors.New("segreto assente: " + name)
	}
	return v, nil
}

func setup(t *testing.T) (*azurefake.Fake, *Plugin) {
	t.Helper()
	f := azurefake.New("sp-secret")
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	d, err := Factory(map[string]any{"management_endpoint": srv.URL, "authority": srv.URL, "scm_endpoint": srv.URL + "/scm", "site_endpoint": srv.URL + "/site"}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Plugin)
	p.poll = 5 * time.Millisecond
	return f, p
}

// pkg writes a zip with version and healthz (fail adds the file that makes OneDeploy fail).
func pkg(t *testing.T, version string, fail bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shop-"+version+".zip")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	files := map[string]string{"version": version, "healthz": "ok"}
	if fail {
		files["fail"] = "1"
	}
	for n, c := range files {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte(c))
	}
	zw.Close()
	out.Close()
	return path
}

func request(version, local string, params map[string]string) *plugin.Request {
	p := map[string]string{"subscription": sub, "resource_group": "rg-shop", "tenant_id": sub, "client_id": sub, "client_secret_secret": "sp"}
	for k, v := range params {
		p[k] = v
	}
	return &plugin.Request{DeploymentID: "d-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "appservice", Name: "shop", Version: version, LocalPath: local},
		Targets:  []plugin.Target{{Host: "shop-weu"}}, Params: p, Secrets: secrets{"sp": "sp-secret"}}
}

func site(f *azurefake.Fake, key string) *azurefake.Site {
	f.Lock()
	defer f.Unlock()
	return f.Sites[key]
}

func TestProduzionePacchetto(t *testing.T) {
	f, p := setup(t)
	f.AddSite("rg-shop", "shop-weu", "")
	ctx := context.Background()
	req := request("1.0.0", pkg(t, "1.0.0", false), map[string]string{ParamHealthPath: "/healthz"})
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	s := site(f, "shop-weu")
	if s.Files["version"] != "1.0.0" || s.Tags[TagVersion] != "1.0.0" || s.Tags[TagDeployment] != "d-1.0.0" || s.Tags[TagPending] != "" {
		t.Errorf("produzione: file %v tag %v", s.Files, s.Tags)
	}
	if len(s.Settings) != 0 {
		t.Errorf("senza slot le app setting non cambiano (riavvio): %v", s.Settings)
	}
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" {
		t.Fatalf("release in esercizio: %+v %v", rel, err)
	}
	// A failing OneDeploy fails the deploy; a package without slot cannot be rolled back.
	bad := request("1.1.0", pkg(t, "1.1.0", true), nil)
	if _, err := p.Deploy(ctx, bad); err == nil || !strings.Contains(err.Error(), "Kudu deployment") {
		t.Errorf("OneDeploy fallito: %v", err)
	}
	if _, err := p.Rollback(ctx, bad, *rel); err == nil || !strings.Contains(err.Error(), "slot") {
		t.Errorf("rollback di un pacchetto senza slot: %v", err)
	}
	if site(f, "shop-weu").Tags[TagPending] != "d-1.1.0" {
		t.Error("produzione toccata senza il tag pending")
	}
}

func TestSlotSwapERollback(t *testing.T) {
	f, p := setup(t)
	f.AddSite("rg-shop", "shop-weu", "")
	f.AddSite("rg-shop", "shop-weu", "staging")
	ctx := context.Background()
	params := map[string]string{ParamSlot: "staging", ParamHealthPath: "/healthz"}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := p.Deploy(ctx, request(v, pkg(t, v, false), params)); err != nil {
			t.Fatal(err)
		}
	}
	prod, slot := site(f, "shop-weu"), site(f, "shop-weu/staging")
	if prod.Files["version"] != "1.1.0" || prod.Settings[SettingVersion] != "1.1.0" || slot.Settings[SettingVersion] != "1.0.0" {
		t.Fatalf("dopo due swap: produzione %v %v, slot %v", prod.Files, prod.Settings, slot.Settings)
	}
	req := request("1.1.0", pkg(t, "1.1.0", false), params)
	// Same deploy again (retry after the swap): no new swap.
	calls := len(f.Calls)
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls[calls:] {
		if strings.HasSuffix(c, "/slotsswap") || strings.Contains(c, "/api/publish") {
			t.Errorf("deploy ripetuto: %s", c)
		}
	}
	rel, _ := p.CurrentRelease(ctx, req)
	if rel == nil || rel.Version != "1.1.0" {
		t.Fatalf("release dall'app setting: %+v", rel)
	}
	if _, err := p.Rollback(ctx, req, plugin.Release{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if v := site(f, "shop-weu").Files["version"]; v != "1.0.0" {
		t.Errorf("swap all'indietro: produzione sulla %s", v)
	}
	// A deploy that did not reach the swap leaves production alone.
	other := request("1.2.0", pkg(t, "1.2.0", false), params)
	if _, err := p.Rollback(ctx, other, plugin.Release{Version: "1.0.0"}); err != nil || site(f, "shop-weu").Files["version"] != "1.0.0" {
		t.Errorf("rollback senza swap: %v", err)
	}
}

func TestRilascioPerPercentuale(t *testing.T) {
	f, p := setup(t)
	f.AddSite("rg-shop", "shop-weu", "")
	f.AddSite("rg-shop", "shop-weu", "staging")
	ctx := context.Background()
	params := map[string]string{ParamSlot: "staging"}
	if _, err := p.Deploy(ctx, request("1.0.0", pkg(t, "1.0.0", false), params)); err != nil {
		t.Fatal(err)
	}
	req := request("2.0.0", pkg(t, "2.0.0", false), params)
	state, err := p.PrepareSlots(ctx, req)
	if err != nil || state["stable@shop-weu"] != "1.0.0" {
		t.Fatalf("stato: %v %v", state, err)
	}
	req.Traffic = &plugin.TrafficRequest{State: state}
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCandidateWeight(ctx, req, 25); err != nil {
		t.Fatal(err)
	}
	prod := site(f, "shop-weu")
	if len(prod.Ramp) != 1 || prod.Ramp[0]["actionHostName"] != "shop-weu-staging.azurewebsites.net" || prod.Ramp[0]["reroutePercentage"] != 25.0 {
		t.Errorf("regola di routing: %v", prod.Ramp)
	}
	if w, err := p.CandidateWeight(ctx, req); err != nil || w != 25 {
		t.Errorf("peso: %d %v", w, err)
	}
	urls, err := p.RouteTesters(ctx, req)
	if err != nil || !strings.HasSuffix(urls["tester_url@shop-weu"], "/site/shop-weu-staging.azurewebsites.net/") {
		t.Errorf("tester: %v %v", urls, err)
	}
	if err := p.PromoteCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	prod = site(f, "shop-weu")
	if prod.Files["version"] != "2.0.0" || len(prod.Ramp) != 0 {
		t.Errorf("promozione: %v ramp %v", prod.Files, prod.Ramp)
	}
	if w, _ := p.CandidateWeight(ctx, req); w != 100 {
		t.Errorf("peso dopo lo swap: %d", w)
	}
	if err := p.RevertCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if v := site(f, "shop-weu").Files["version"]; v != "1.0.0" {
		t.Errorf("ritorno alla stabile: %s", v)
	}
	if _, err := p.PrepareSlots(ctx, request("2.0.0", "", nil)); err == nil {
		t.Error("strategy traffic senza slot accettata")
	}
}

func TestImmagine(t *testing.T) {
	f, p := setup(t)
	f.AddSite("rg-shop", "shop-weu", "").LinuxFx = "DOCKER|registry.example.com/shop:0.9.0"
	ctx := context.Background()
	digest := strings.Repeat("ab", 32)
	ref := filepath.Join(t.TempDir(), "shop"+plugin.ImageRefExt)
	if err := os.WriteFile(ref, []byte("registry.example.com/shop@sha256:"+digest), 0o600); err != nil {
		t.Fatal(err)
	}
	req := request("1.0.0", ref, nil)
	req.Artifact.SHA256 = digest
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	s := site(f, "shop-weu")
	if s.LinuxFx != "DOCKER|registry.example.com/shop@sha256:"+digest || s.Tags[TagImage] == "" {
		t.Fatalf("immagine: %s %v", s.LinuxFx, s.Tags)
	}
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" || rel.Details["image@shop-weu"] == "" {
		t.Fatalf("release: %+v %v", rel, err)
	}
	// Rollback to the previous image, read at the checkpoint.
	prev := plugin.Release{Version: "0.9.0", Details: map[string]string{"image@shop-weu": "registry.example.com/shop:0.9.0"}}
	if _, err := p.Rollback(ctx, req, prev); err != nil {
		t.Fatal(err)
	}
	if s := site(f, "shop-weu"); s.LinuxFx != "DOCKER|registry.example.com/shop:0.9.0" || s.Tags[TagVersion] != "0.9.0" {
		t.Errorf("rollback dell'immagine: %s %v", s.LinuxFx, s.Tags)
	}
	// An image changed outside manent is no release.
	f.Lock()
	f.Sites["shop-weu"].LinuxFx = "DOCKER|registry.example.com/shop:hotfix"
	f.Unlock()
	if rel, err := p.CurrentRelease(ctx, req); err != nil || rel != nil {
		t.Errorf("immagine cambiata a mano: %+v %v", rel, err)
	}
}

func TestValidazione(t *testing.T) {
	_, p := setup(t)
	req := request("1.0.0", "/tmp/shop.tar", map[string]string{"subscription": "x", ParamSlot: "Production", "client_secret_secret": "sp", "tenant_id": ""})
	req.Targets = []plugin.Target{{Host: "a/b/c"}}
	err := p.Validate(req)
	for _, want := range []string{".zip", "subscription", "slot", "tenant_id", "invalid resource name"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("manca %q in %v", want, err)
		}
	}
	f, p := setup(t)
	_ = f
	if _, err := p.Deploy(context.Background(), request("1.0.0", pkg(t, "1.0.0", false), nil)); err == nil || !strings.Contains(err.Error(), "infrastructure") {
		t.Errorf("web app assente: %v", err)
	}
	wrong := request("1.0.0", pkg(t, "1.0.0", false), nil)
	wrong.Secrets = secrets{"sp": "wrong"}
	if _, err := p.Deploy(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("segreto errato: %v", err)
	}
}
