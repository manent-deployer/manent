package rollout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
)

func TestHealthGate(t *testing.T) {
	errorRate := "0.002"
	alerts := `[]`
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		switch {
		case strings.Contains(q, `az="eu-1b"`):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + errorRate + `"]}]}}`))
		}
	}))
	defer prom.Close()
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(alerts))
	}))
	defer am.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer target.Close()
	host := strings.TrimPrefix(target.URL, "http://")

	c := &HTTPChecker{Client: http.DefaultClient, PrometheusURL: prom.URL, AlertmanagerURL: am.URL}
	maxRate := 0.01
	h := descriptor.Health{
		Probes:  []descriptor.Probe{{URL: "http://{host}/healthz"}},
		Metrics: []descriptor.MetricGate{{Name: "error-rate", Query: `rate(errors{app="{application}",az="{az}"}[5m])`, Max: &maxRate}},
		Alerts:  &descriptor.AlertGate{BlockOn: []string{"severity=critical"}},
	}
	pass := Pass{Region: "eu-1", AZ: "eu-1a", Targets: []descriptor.Target{{Host: host}}}
	in := GateInput{Application: "myapp", Environment: "prod", Passes: []Pass{pass}, Health: h}
	ctx := context.Background()

	if r := c.Check(ctx, in); !r.OK || len(r.Checks) != 3 {
		t.Fatalf("gate verde atteso: %+v", r)
	}
	errorRate = "0.05"
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "error-rate (eu-1/eu-1a): valore 0.05 fuori soglia") {
		t.Fatalf("soglia superata non rilevata: %s", r.Failed())
	}
	errorRate = "0.002"
	// Critical alert of the same application in another region: it does not block eu-1.
	alerts = `[{"labels":{"alertname":"HighLatency","severity":"critical","app":"myapp","region":"us-1"}}]`
	if r := c.Check(ctx, in); !r.OK {
		t.Fatalf("allarme di un'altra region: %s", r.Failed())
	}
	alerts = `[{"labels":{"alertname":"HighLatency","severity":"critical","app":"myapp","region":"eu-1"}}]`
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "HighLatency") {
		t.Fatalf("allarme non rilevato: %s", r.Failed())
	}
	alerts = `[]`
	// No data: red.
	in.Passes[0].AZ = "eu-1b"
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "nessun dato") {
		t.Fatalf("nessun dato accettato: %s", r.Failed())
	}
	// Failed probe.
	in.Passes[0].AZ = "eu-1a"
	in.Health.Probes[0].URL = "http://{host}/ready"
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "stato 503") {
		t.Fatalf("sonda: %s", r.Failed())
	}
	if (&HTTPChecker{}).Supports(&h) == nil {
		t.Error("metriche e allarmi accettati senza Prometheus e Alertmanager")
	}
}

func TestGateComparativo(t *testing.T) {
	values := map[string]string{"b": "0.03", "a": "0.01", "samples": "500"}
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		v := values["samples"]
		switch {
		case strings.Contains(q, `track="b"`) && strings.Contains(q, "errors"):
			v = values["b"]
		case strings.Contains(q, `track="a"`) && strings.Contains(q, "errors"):
			v = values["a"]
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + v + `"]}]}}`))
	}))
	defer prom.Close()
	c := &HTTPChecker{Client: http.DefaultClient, PrometheusURL: prom.URL}
	ratio := 2.0
	h := descriptor.Health{Metrics: []descriptor.MetricGate{{Name: "errori", Query: `rate(errors{app="{application}",track="{track}"}[5m])`,
		Compare: descriptor.CompareStable, MaxRatio: &ratio, SamplesQuery: `sum(requests{track="{track}"})`, MinSamples: 200}}}
	in := GateInput{Application: "shop", Passes: []Pass{{Name: "traffic"}}, Health: h, Track: "b", StableTrack: "a", Version: "2.0.0"}
	ctx := context.Background()
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "rapporto 3.00 oltre 2") {
		t.Fatalf("candidata peggiore della stabile non rilevata: %s", r.Failed())
	}
	values["b"] = "0.015"
	if r := c.Check(ctx, in); !r.OK {
		t.Fatalf("gate verde atteso: %s", r.Failed())
	}
	// Few samples: the gate cannot say the candidate is healthy.
	values["samples"] = "50"
	if r := c.Check(ctx, in); r.OK || !strings.Contains(r.Failed(), "campioni insufficienti") {
		t.Fatalf("campioni: %s", r.Failed())
	}
	// First release: nothing to compare.
	values["samples"], in.StableTrack = "500", ""
	if r := c.Check(ctx, in); !r.OK {
		t.Fatalf("primo rilascio: %s", r.Failed())
	}
}
