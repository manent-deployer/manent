package lambda

import (
	"context"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// routing legge i pesi aggiuntivi dell'alias live dal finto Lambda.
func (f *fakeLambda) routing(fn string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, _ := f.functions[fn].aliases["live"]["RoutingConfig"].(map[string]any)
	w, _ := rc["AdditionalVersionWeights"].(map[string]any)
	return w
}

func TestAliasPesato(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	if _, err := p.Deploy(ctx, imageRequest(t, "1.0.0", "shop")); err != nil {
		t.Fatal(err)
	}
	req := imageRequest(t, "2.0.0", "shop-v2")
	req.Traffic = &plugin.TrafficRequest{}
	state, err := p.PrepareSlots(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if state[plugin.StateHasStable] != "true" || state["stable@eu-west-1"] != "1" {
		t.Fatalf("stato: %v", state)
	}
	req.Traffic.State = state
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.alias("shop") != "1" {
		t.Fatalf("la candidata ha spostato l'alias: %s", f.alias("shop"))
	}
	if out, err := p.RouteTesters(ctx, req); err != nil || out["tester_qualifier@eu-west-1"] != "2" {
		t.Errorf("tester: %v %v", out, err)
	}
	if err := p.SetCandidateWeight(ctx, req, 25); err != nil {
		t.Fatal(err)
	}
	if w := f.routing("shop"); f.alias("shop") != "1" || w["2"] != 0.25 {
		t.Fatalf("alias %s, pesi %v", f.alias("shop"), w)
	}
	if w, err := p.CandidateWeight(ctx, req); err != nil || w != 25 {
		t.Errorf("peso letto: %d %v", w, err)
	}
	// Durante il rilascio la release in esercizio resta la stabile.
	if cur, err := p.CurrentRelease(ctx, req); err != nil || cur.Version != "1.0.0" {
		t.Errorf("release durante il rilascio: %+v %v", cur, err)
	}
	if err := p.RevertCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.alias("shop") != "1" || len(f.routing("shop")) != 0 {
		t.Errorf("ritorno: alias %s, pesi %v", f.alias("shop"), f.routing("shop"))
	}
	if err := p.SetCandidateWeight(ctx, req, 50); err != nil {
		t.Fatal(err)
	}
	// Un nuovo rilascio non parte mentre l'alias divide il traffico.
	if _, err := p.PrepareSlots(ctx, req); err == nil {
		t.Error("rilascio preparato con l'alias diviso")
	}
	if err := p.PromoteCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.alias("shop") != "2" || len(f.routing("shop")) != 0 {
		t.Errorf("promozione: alias %s, pesi %v", f.alias("shop"), f.routing("shop"))
	}
	if cur, _ := p.CurrentRelease(ctx, req); cur == nil || cur.Version != "2.0.0" {
		t.Errorf("release dopo la promozione: %+v", cur)
	}
	if err := p.SetCandidateWeight(ctx, imageRequestWith(t, "3.0.0", "shop-v3", req), 10); err == nil {
		t.Error("peso impostato per una versione mai pubblicata")
	}
}

func imageRequestWith(t *testing.T, version, image string, prev *plugin.Request) *plugin.Request {
	r := imageRequest(t, version, image)
	r.Traffic = prev.Traffic
	return r
}

func TestPrimoRilascioConAlias(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	req := imageRequest(t, "1.0.0", "shop")
	req.Traffic = &plugin.TrafficRequest{}
	state, err := p.PrepareSlots(ctx, req)
	if err != nil || state[plugin.StateHasStable] != "false" {
		t.Fatalf("stato: %v %v", state, err)
	}
	req.Traffic.State = state
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Senza versione stabile l'alias viene creato direttamente sulla candidata.
	if err := p.SetCandidateWeight(ctx, req, 100); err != nil || f.alias("shop") != "1" {
		t.Fatalf("alias %s: %v", f.alias("shop"), err)
	}
}
