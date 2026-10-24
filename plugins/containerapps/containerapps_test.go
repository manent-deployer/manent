package containerapps

import (
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
	d, err := Factory(map[string]any{"management_endpoint": srv.URL, "authority": srv.URL}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Plugin)
	p.poll = 5 * time.Millisecond
	return f, p
}

// request is a deploy of the image registry.example.com/<name>@sha256:<digest of version>.
func request(t *testing.T, name, version string) *plugin.Request {
	t.Helper()
	digest := strings.Repeat(version[:1], 64)
	ref := filepath.Join(t.TempDir(), "img"+plugin.ImageRefExt)
	if err := os.WriteFile(ref, []byte("registry.example.com/"+name+"@sha256:"+digest), 0o600); err != nil {
		t.Fatal(err)
	}
	return &plugin.Request{DeploymentID: "d-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "containerapps", Name: "shop", Version: version, LocalPath: ref, SHA256: digest},
		Targets:  []plugin.Target{{Host: "rg-shop/shop-api"}},
		Params:   map[string]string{"subscription": sub, "tenant_id": sub, "client_id": sub, "client_secret_secret": "sp"},
		Secrets:  secrets{"sp": "sp-secret"}}
}

func appState(f *azurefake.Fake) *azurefake.App {
	f.Lock()
	defer f.Unlock()
	return f.Apps["shop-api"]
}

func TestDeployERollbackMultiple(t *testing.T) {
	f, p := setup(t)
	f.AddApp("rg-shop", "shop-api", "Multiple", "api", "registry.example.com/shop:legacy")
	ctx := context.Background()
	r1 := request(t, "shop", "1.0.0")
	if err := p.Validate(r1); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(ctx, r1); err != nil {
		t.Fatal(err)
	}
	r2 := request(t, "shop", "2.0.0")
	prev, err := p.CurrentRelease(ctx, r2)
	if err != nil || prev == nil || prev.Version != "1.0.0" {
		t.Fatalf("release in esercizio: %+v %v", prev, err)
	}
	res, err := p.Deploy(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	cand := "shop-api--" + candidateSuffix(r2)
	a := appState(f)
	if a.LatestReady != cand || res.Targets[0].Details["previous"] != "1.0.0" {
		t.Fatalf("revisione: %s %v", a.LatestReady, res.Targets[0].Details)
	}
	// Retry: no new revision.
	patches := a.Patches
	if _, err := p.Deploy(ctx, r2); err != nil || appState(f).Patches != patches {
		t.Errorf("deploy ripetuto: %v, patch %d -> %d", err, patches, appState(f).Patches)
	}
	if _, err := p.Rollback(ctx, r2, *prev); err != nil {
		t.Fatal(err)
	}
	a = appState(f)
	if len(a.Traffic) != 1 || a.Traffic[0].(map[string]any)["revisionName"] != prev.Details["revision@rg-shop/shop-api"] {
		t.Errorf("traffico dopo il rollback: %v", a.Traffic)
	}
	if rel, _ := p.CurrentRelease(ctx, r2); rel == nil || rel.Version != "1.0.0" {
		t.Errorf("release dopo il rollback: %+v", rel)
	}
	// A broken image fails on the revision health.
	bad := request(t, "shop-broken", "3.0.0")
	if _, err := p.Deploy(ctx, bad); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("revisione non sana: %v", err)
	}
}

func TestRollbackSingle(t *testing.T) {
	f, p := setup(t)
	f.AddApp("rg-shop", "shop-api", "Single", "api", "registry.example.com/shop:legacy")
	ctx := context.Background()
	r1 := request(t, "shop", "1.0.0")
	if _, err := p.Deploy(ctx, r1); err != nil {
		t.Fatal(err)
	}
	r2 := request(t, "shop", "2.0.0")
	prev, _ := p.CurrentRelease(ctx, r2)
	if _, err := p.Deploy(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rollback(ctx, r2, *prev); err != nil {
		t.Fatal(err)
	}
	a := appState(f)
	if !strings.Contains(a.LatestReady, "--r") || !strings.Contains(a.Template["containers"].([]any)[0].(map[string]any)["image"].(string), "@sha256:1111") {
		t.Errorf("Single: nuova revisione con l'immagine precedente attesa, %s", a.LatestReady)
	}
	if rel, _ := p.CurrentRelease(ctx, r2); rel == nil || rel.Version != "1.0.0" {
		t.Errorf("release dopo il rollback: %+v", rel)
	}
	// Traffic rollouts require Multiple.
	if _, err := p.PrepareSlots(ctx, r2); err == nil || !strings.Contains(err.Error(), "Multiple") {
		t.Errorf("Single accettato per strategy traffic: %v", err)
	}
}

func TestRilascioPerPercentuale(t *testing.T) {
	f, p := setup(t)
	f.AddApp("rg-shop", "shop-api", "Multiple", "api", "registry.example.com/shop:legacy")
	ctx := context.Background()
	if _, err := p.Deploy(ctx, request(t, "shop", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	req := request(t, "shop", "2.0.0")
	state, err := p.PrepareSlots(ctx, req)
	if err != nil || state[plugin.StateHasStable] != "true" {
		t.Fatalf("stato: %v %v", state, err)
	}
	req.Traffic = &plugin.TrafficRequest{State: state, TesterTag: "canary"}
	stable := state["stable@rg-shop/shop-api"]
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if w, _ := p.CandidateWeight(ctx, req); w != 0 {
		t.Errorf("candidata con traffico prima dei gradini: %d", w)
	}
	if err := p.SetCandidateWeight(ctx, req, 30); err != nil {
		t.Fatal(err)
	}
	if w, _ := p.CandidateWeight(ctx, req); w != 30 {
		t.Errorf("peso: %d", w)
	}
	urls, err := p.RouteTesters(ctx, req)
	if err != nil || !strings.HasPrefix(urls["tester_url@rg-shop/shop-api"], "https://shop-api---canary.") {
		t.Errorf("tester: %v %v", urls, err)
	}
	if err := p.PromoteCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if rel, _ := p.CurrentRelease(ctx, req); rel == nil || rel.Version != "2.0.0" {
		t.Errorf("promozione: %+v", rel)
	}
	if err := p.RetirePrevious(ctx, req); err != nil {
		t.Fatal(err)
	}
	if appState(f).Revisions[stable].Active {
		t.Error("revisione sostituita ancora attiva")
	}
	if err := p.RevertCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if rel, _ := p.CurrentRelease(ctx, req); rel == nil || rel.Version != "1.0.0" {
		t.Errorf("ritorno alla stabile riattivata: %+v", rel)
	}
}

func TestValidazione(t *testing.T) {
	_, p := setup(t)
	req := request(t, "shop", "1.0.0")
	req.Artifact.LocalPath = "/tmp/shop.zip"
	req.Params["subscription"] = ""
	req.Targets = []plugin.Target{{Host: "Shop_API"}}
	err := p.Validate(req)
	for _, want := range []string{"registry image", "subscription", "resource group"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("manca %q in %v", want, err)
		}
	}
	if _, err := p.Deploy(context.Background(), request(t, "shop", "1.0.0")); err == nil || !strings.Contains(err.Error(), "infrastructure") {
		t.Errorf("app assente: %v", err)
	}
}
