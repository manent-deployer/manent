package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

const routePath = "/apis/gateway.networking.k8s.io/v1/namespaces/shop/httproutes/shop"

// seedRoute creates the infrastructure HTTPRoute: one rule for testers and one split between the slots.
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

// routeState summarizes the route: testers' backend and weights of the main rule.
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

// prepare prepares the slots and releases the candidate; it returns the request with the state.
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

	// First release: no stable version, the candidate goes to slot a and gets everything.
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

	// Second release: stable a, candidate b, testers and steps.
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
	// A patch with an outdated resourceVersion is rejected instead of overwriting.
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

	// Third release, failed: traffic and testers go back to slot b, the candidate goes to zero.
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
	// The delayed scale-down of a release replaced meanwhile touches nothing.
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
	objs, err = slotObjects(objs, "b", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if objs[0].Name() != "db-b" || objs[0].str("spec", "serviceName") != "db-b" || objs[1].str("spec", "selector", LabelTrack) != "b" ||
		objs[2].str("spec", "scaleTargetRef", "name") != "db-b" || objs[3].Name() != "shared" {
		t.Errorf("oggetti dello slot: %v", objs)
	}
}

const vsPath = "/apis/networking.istio.io/v1/namespaces/shop/virtualservices/shop"

func TestSlotEIstio(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)
	fc.mu.Lock()
	fc.objects[vsPath] = Object{
		"apiVersion": istioAPIVersion, "kind": "VirtualService",
		"metadata": map[string]any{"name": "shop", "namespace": "shop", "resourceVersion": "1"},
		"spec": map[string]any{"hosts": []any{"shop"}, "http": []any{
			map[string]any{"match": []any{map[string]any{"headers": map[string]any{"x-canary": map[string]any{"exact": "1"}}}},
				"route": []any{map[string]any{"destination": map[string]any{"host": "shop", "subset": "a"}}}},
			map[string]any{"route": []any{
				map[string]any{"destination": map[string]any{"host": "shop", "subset": "a"}, "weight": float64(100)},
				map[string]any{"destination": map[string]any{"host": "shop", "subset": "b"}, "weight": float64(0)},
			}},
		}},
	}
	fc.mu.Unlock()
	req := request(server, "1.0.0", manifestV1, t)
	req.Params[ParamTrafficRouter], req.Params[ParamTrafficRoute] = RouterIstio, "shop"
	req.Traffic = &plugin.TrafficRequest{TesterHeader: "x-canary: 1", State: map[string]string{"stable@" + server: "a", "candidate@" + server: "b"}}
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	// With Istio the Service stays shared by both slots; pods carry the slot label.
	if svc := fc.get(servicePath); svc == nil || svc.str("spec", "selector", LabelTrack) != "" {
		t.Errorf("service comune: %v", svc)
	}
	if fc.get("/apis/apps/v1/namespaces/shop/deployments/shop-b").str("spec", "template", "metadata", "labels", LabelTrack) != "b" {
		t.Error("pod senza l'etichetta dello slot")
	}
	if _, err := p.RouteTesters(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCandidateWeight(ctx, req, 30); err != nil {
		t.Fatal(err)
	}
	if w, err := p.CandidateWeight(ctx, req); err != nil || w != 30 {
		t.Errorf("peso: %d %v", w, err)
	}
	http := rulesOf(fc.get(vsPath), "http")
	tester := http[0].(map[string]any)["route"].([]any)[0].(map[string]any)["destination"].(map[string]any)["subset"]
	main := http[1].(map[string]any)["route"].([]any)
	if tester != "b" || main[0].(map[string]any)["weight"] != float64(70) || main[1].(map[string]any)["weight"] != float64(30) {
		t.Errorf("virtual service: %v", http)
	}
}

func TestSlotChartConNomiPropri(t *testing.T) {
	objs, err := parseObjects([]byte(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: shop-b-web}
spec: {selector: {matchLabels: {app: web}}, template: {metadata: {labels: {app: web}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	// With traffic_rename: "false" the chart already names objects per slot: manent only adds labels.
	objs, _ = slotObjects(objs, "b", true, false)
	if objs[0].Name() != "shop-b-web" || objs[0].str("spec", "selector", "matchLabels", LabelTrack) != "b" {
		t.Errorf("oggetto: %v", objs[0])
	}
}

func TestRepliceProporzionali(t *testing.T) {
	ctx := context.Background()
	p, fc, server := setup(t)
	seedRoute(fc)
	r1 := prepare(t, p, server, "1.0.0")
	if err := p.PromoteCandidate(ctx, r1); err != nil {
		t.Fatal(err)
	}
	req := trafficRequest(server, "2.0.0", t)
	req.Params["var.REPLICAS"] = "10"
	state, err := p.PrepareSlots(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Traffic.State, req.Traffic.ProportionalReplicas = state, true
	if _, err := p.DeployCandidate(ctx, req); err != nil {
		t.Fatal(err)
	}
	const cand = "/apis/apps/v1/namespaces/shop/deployments/shop-b"
	if d := fc.get(cand); num(d, "spec", "replicas") != 1 || d.str("metadata", "annotations", AnnotationReplicas) != "10" {
		t.Fatalf("candidata avviata piccola: %v", d)
	}
	for pct, want := range map[int]int{25: 3, 100: 10} {
		if err := p.ScaleCandidate(ctx, req, pct); err != nil {
			t.Fatal(err)
		}
		if n := num(fc.get(cand), "spec", "replicas"); n != want {
			t.Errorf("al %d%%: %d repliche, attese %d", pct, n, want)
		}
	}
}
