package cloudrun

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// split restituisce il traffico del finto servizio come "revisione=percentuale".
func (f *fakeRun) split() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for _, raw := range f.svc["traffic"].([]any) {
		m := raw.(map[string]any)
		r, _ := m["revision"].(string)
		if m["type"] == trafficLatest {
			r = "LATEST"
		}
		parts = append(parts, fmt.Sprintf("%s=%v", r, m["percent"]))
	}
	return strings.Join(parts, " ")
}

func TestRevisioneCandidata(t *testing.T) {
	p, f, key := setup(t)
	ctx := context.Background()
	if _, err := p.Deploy(ctx, request(t, key, "1.0.0", "europe-docker.pkg.dev/acme/shop/shop@"+digest)); err != nil {
		t.Fatal(err)
	}
	stable := f.traffic()
	req := request(t, key, "2.0.0", "europe-docker.pkg.dev/acme/shop/shop-v2@"+digest)
	req.Traffic = &plugin.TrafficRequest{TesterTag: "candidate"}
	state, err := p.PrepareSlots(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if state[plugin.StateHasStable] != "true" || state["stable@europe-west1"] != stable {
		t.Fatalf("stato: %v", state)
	}
	req.Traffic.State = state
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	cand := candidateRevision(req)
	if got := f.split(); got != stable+"=100 "+cand+"=0" {
		t.Fatalf("traffico dopo il deploy della candidata: %s", got)
	}
	// Ripetuto, il deploy della candidata non crea un'altra revisione.
	n := len(f.revisions)
	if _, err := p.DeployCandidate(ctx, req); err != nil || len(f.revisions) != n {
		t.Fatalf("deploy ripetuto: %v, revisioni %d -> %d", err, n, len(f.revisions))
	}
	if out, err := p.RouteTesters(ctx, req); err != nil || !strings.HasPrefix(out["tester_url@europe-west1"], "https://candidate---") {
		t.Errorf("tester: %v %v", out, err)
	}
	if err := p.SetCandidateWeight(ctx, req, 10); err != nil {
		t.Fatal(err)
	}
	if got := f.split(); got != stable+"=90 "+cand+"=10" {
		t.Errorf("traffico al 10%%: %s", got)
	}
	if w, err := p.CandidateWeight(ctx, req); err != nil || w != 10 {
		t.Errorf("peso letto: %d %v", w, err)
	}
	// Con il traffico diviso la release in esercizio resta la stabile.
	if cur, err := p.CurrentRelease(ctx, req); err != nil || cur.Version != "1.0.0" {
		t.Errorf("release durante il rilascio: %+v %v", cur, err)
	}
	if err := p.RevertCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := f.split(); got != stable+"=100" {
		t.Errorf("traffico dopo il ritorno: %s", got)
	}
	if err := p.PromoteCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := f.split(); got != "LATEST=100" || f.traffic() != cand {
		t.Errorf("traffico dopo la promozione: %s (%s)", got, f.traffic())
	}
	if cur, _ := p.CurrentRelease(ctx, req); cur == nil || cur.Version != "2.0.0" {
		t.Errorf("release dopo la promozione: %+v", cur)
	}
}
