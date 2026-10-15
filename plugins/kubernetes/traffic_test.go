package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

const routePath = "/apis/gateway.networking.k8s.io/v1/namespaces/shop/httproutes/shop"

// seedRoute crea la HTTPRoute dell'infrastruttura: una regola per i tester e una divisa tra gli slot.
func seedRoute(fc *fakeCluster) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.rv++
	fc.objects[routePath] = Object{
		"apiVersion": gatewayAPIVersion, "kind": "HTTPRoute",
		"metadata": map[string]any{"name": "shop", "namespace": "shop", "resourceVersion": fmt.Sprint(fc.rv)},
		"spec": map[string]any{"rules": []any{
			map[string]any{"matches": []any{map[string]any{"headers": []any{map[string]any{"name": "X-Canary", "value": "1"}}}},
				"backendRefs": []any{map[string]any{"name": "shop-a", "port": float64(80)}}},
			map[string]any{"backendRefs": []any{map[string]any{"name": "shop-a", "port": float64(80), "weight": float64(100)}, map[string]any{"name": "shop-b", "port": float64(80), "weight": float64(0)}}},
		}},
	}
}

// routeState riassume la route: backend dei tester e pesi della regola principale.
func routeState(fc *fakeCluster) string {
	r := fc.get(routePath)
	rules := rulesOf(r, "rules")
	tester := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["name"]
	var ws []string
	for _, ref := range rules[1].(map[string]any)["backendRefs"].([]any) {
		m := ref.(map[string]any)
		ws = append(ws, fmt.Sprintf("%s=%v", m["name"], m["weight"]))
	}
	return fmt.Sprintf("tester=%s %s", tester, strings.Join(ws, " "))
}

func trafficRequest(server, version string, t *testing.T) *plugin.Request {
	req := request(server, version, manifestV1, t)
	req.Params[ParamTrafficRoute], req.Params[ParamTrafficService] = "shop", "shop"
	req.Traffic = &plugin.TrafficRequest{TesterHeader: "x-canary: 1"}
	return req
}

// prepare prepara gli slot e rilascia la candidata; restituisce la richiesta con lo stato.
func prepare(t *testing.T, p *Plugin, server, version string) *plugin.Request {
	t.Helper()
	ctx := context.Background()
	req := trafficRequest(server, version, t)
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	state, err := p.PrepareSlots(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Traffic.State = state
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSlotEGatewayAPI(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)
	seedRoute(fc)

	// Primo rilascio: nessuna versione stabile, la candidata va nello slot a e riceve tutto.
	r1 := prepare(t, p, server, "1.0.0")
	if r1.Traffic.State[plugin.StateHasStable] != "false" || r1.Traffic.State["candidate"] != "a" {
		t.Fatalf("stato: %v", r1.Traffic.State)
	}
	dep := fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-a")
	if dep == nil || dep.str("spec", "selector", "matchLabels", LabelTrack) != "a" || dep.str("spec", "template", "metadata", "labels", LabelTrack) != "a" {
		t.Fatalf("deployment dello slot: %v", dep)
	}
	if svc := fc.get("/api/v1/namespaces/shop/services/shop-a"); svc == nil {
		t.Fatal("service dello slot assente")
	}
	if fc.get(deployPath) != nil {
		t.Error("deployment senza slot creato")
	}
	if err := p.SetCandidateWeight(ctx, r1, 100); err != nil {
		t.Fatal(err)
	}
	if err := p.PromoteCandidate(ctx, r1); err != nil {
		t.Fatal(err)
	}
	if got := fc.get(pointerPath).str("data", dataSlot); got != "a" {
		t.Fatalf("slot in esercizio: %q", got)
	}

	// Secondo rilascio: stabile a, candidata b, tester e gradini.
	r2 := prepare(t, p, server, "2.0.0")
	if r2.Traffic.State["stable"] != "a" || r2.Traffic.State["candidate"] != "b" || r2.Traffic.State[plugin.StateHasStable] != "true" {
		t.Fatalf("stato: %v", r2.Traffic.State)
	}
	if fc.get(pointerPath).str("data", dataVersion) != "1.0.0" {
		t.Error("la candidata ha cambiato la release in esercizio")
	}
	if _, err := p.RouteTesters(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCandidateWeight(ctx, r2, 25); err != nil {
		t.Fatal(err)
	}
	if got := routeState(fc); got != "tester=shop-b shop-a=75 shop-b=25" {
		t.Errorf("route: %s", got)
	}
	if w, err := p.CandidateWeight(ctx, r2); err != nil || w != 25 {
		t.Errorf("peso letto: %d %v", w, err)
	}
	// Una patch con un resourceVersion superato è rifiutata invece di sovrascrivere.
	api, err := p.connect(ctx, r2.Targets[0], r2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Patch(ctx, gatewayAPIVersion, "HTTPRoute", "shop", "shop", map[string]any{"metadata": map[string]any{"resourceVersion": "1"}}); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Errorf("patch con versione superata: %v", err)
	}
	if err := p.PromoteCandidate(ctx, r2); err != nil {
		t.Fatal(err)
	}
	pt := fc.get(pointerPath)
	if pt.str("data", dataSlot) != "b" || pt.str("data", dataPreviousSlot) != "a" || pt.str("data", dataPreviousVersion) != "1.0.0" {
		t.Errorf("storico dopo la promozione: %v", pt["data"])
	}
	if cur, err := p.CurrentRelease(ctx, r2); err != nil || cur.Version != "2.0.0" {
		t.Errorf("release in esercizio: %+v %v", cur, err)
	}
	if err := p.RetirePrevious(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if n := num(fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-a"), "spec", "replicas"); n != 0 {
		t.Errorf("slot sostituito non ridimensionato: %d repliche", n)
	}
	if n := num(fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-b"), "spec", "replicas"); n != 1 {
		t.Errorf("slot in esercizio toccato: %d repliche", n)
	}

	// Terzo rilascio, fallito: traffico e tester tornano allo slot b, la candidata va a zero.
	r3 := prepare(t, p, server, "3.0.0")
	if _, err := p.RouteTesters(ctx, r3); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCandidateWeight(ctx, r3, 10); err != nil {
		t.Fatal(err)
	}
	if err := p.RevertCandidate(ctx, r3); err != nil {
		t.Fatal(err)
	}
	if got := routeState(fc); got != "tester=shop-b shop-a=0 shop-b=100" {
		t.Errorf("route dopo il ritorno: %s", got)
	}
	if n := num(fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-a"), "spec", "replicas"); n != 0 {
		t.Errorf("candidata non ridimensionata: %d", n)
	}
	if fc.get(pointerPath).str("data", dataVersion) != "2.0.0" {
		t.Error("release in esercizio cambiata dal ritorno")
	}
	// Il ridimensionamento ritardato di un rilascio sostituito nel frattempo non tocca nulla.
	if err := p.RetirePrevious(ctx, r3); err != nil {
		t.Fatal(err)
	}
	if n := num(fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-b"), "spec", "replicas"); n != 1 {
		t.Errorf("slot in esercizio ridimensionato: %d", n)
	}
}

func TestSlotValidazione(t *testing.T) {
	req := request("https://k8s", "1.0.0", manifestV1, t)
	req.Traffic = &plugin.TrafficRequest{}
	err := (&Plugin{}).Validate(req)
	if err == nil || !strings.Contains(err.Error(), ParamTrafficService) || !strings.Contains(err.Error(), ParamTrafficRoute) {
		t.Errorf("validazione: %v", err)
	}
	objs, err := parseObjects([]byte(`
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db}
spec: {serviceName: db, selector: {matchLabels: {app: db}}, template: {metadata: {labels: {app: db}}}}
---
apiVersion: v1
kind: Service
metadata: {name: db}
spec: {selector: {app: db}}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: db}
spec: {scaleTargetRef: {kind: StatefulSet, name: db}}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: shared}
`))
	if err != nil {
		t.Fatal(err)
	}
	objs, err = slotObjects(objs, "b", true)
	if err != nil {
		t.Fatal(err)
	}
	if objs[0].Name() != "db-b" || objs[0].str("spec", "serviceName") != "db-b" || objs[1].str("spec", "selector", LabelTrack) != "b" ||
		objs[2].str("spec", "scaleTargetRef", "name") != "db-b" || objs[3].Name() != "shared" {
		t.Errorf("oggetti dello slot: %v", objs)
	}
}
