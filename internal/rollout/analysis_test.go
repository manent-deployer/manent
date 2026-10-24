package rollout

import (
	"context"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
)

// fakeSource returns fixed series per track ({track} in the query) and records the queries.
type fakeSource struct {
	series  map[string][]float64
	queries []string
}

func (f *fakeSource) Value(_ context.Context, q string, _ time.Time) (float64, error) {
	f.queries = append(f.queries, q)
	v := f.series[q]
	if len(v) == 0 {
		return 0, ErrNoData
	}
	return v[len(v)-1], nil
}

func (f *fakeSource) Series(_ context.Context, q string, _, _ time.Time, _ time.Duration) ([]float64, error) {
	f.queries = append(f.queries, q)
	return f.series[q], nil
}

// noisy returns n values around base.
func noisy(r *rand.Rand, n int, base float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = base * (0.9 + 0.2*r.Float64())
	}
	return out
}

func TestAnalisiCanary(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	src := &fakeSource{series: map[string][]float64{}}
	c := &HTTPChecker{Client: http.DefaultClient, Sources: map[string]MetricSource{"datadog": src}}
	errors := descriptor.MetricGate{Name: "errors", Provider: "datadog", Compare: descriptor.CompareStable, Analysis: descriptor.AnalysisMannWhitney,
		Query: `errors{track="{track}"}`, Critical: true}
	latency := descriptor.MetricGate{Name: "latency", Provider: "datadog", Compare: descriptor.CompareStable, Analysis: descriptor.AnalysisMannWhitney,
		Query: `latency{track="{track}"}`}
	in := GateInput{Application: "shop", Track: "b", StableTrack: "a", Elapsed: 5 * time.Minute, Passes: []Pass{{}},
		Health: descriptor.Health{Metrics: []descriptor.MetricGate{errors, latency}, Analysis: &descriptor.CanaryAnalysis{}}}
	// set gives the series of a track to both metrics.
	set := func(track string, v []float64) {
		src.series[`errors{track="`+track+`"}`], src.series[`latency{track="`+track+`"}`] = v, v
	}
	ctx := context.Background()

	// Same distribution: no significant difference, score 100.
	sameA, sameB := noisy(r, 20, 0.01), noisy(r, 20, 0.01)
	set("a", sameA)
	set("b", sameB)
	res := c.Check(ctx, in)
	if !res.OK || res.Score == nil || *res.Score != 100 {
		t.Fatalf("stessa distribuzione: %+v", res)
	}
	if res.Checks[0].PValue == nil || *res.Checks[0].PValue < 0.05 {
		t.Errorf("p-value: %+v", res.Checks[0])
	}
	// Candidate clearly worse on the critical metric: red whatever the score.
	set("b", noisy(r, 20, 0.05))
	res = c.Check(ctx, in)
	if res.OK || !strings.Contains(res.Failed(), "errors") || !strings.Contains(res.Failed(), "score 0/100") {
		t.Fatalf("candidata peggiore: %s", res.Failed())
	}
	// Candidate better (lower values) with direction increase: green.
	set("b", noisy(r, 20, 0.002))
	if res = c.Check(ctx, in); !res.OK {
		t.Fatalf("candidata migliore: %s", res.Failed())
	}
	// Few points: inconclusive and green during the bake, red at its end.
	set("b", []float64{0.01, 0.01})
	res = c.Check(ctx, in)
	if !res.OK || !res.Checks[0].Inconclusive || res.Score != nil {
		t.Fatalf("pochi punti durante il bake: %+v", res)
	}
	in.Final = true
	if res = c.Check(ctx, in); res.OK {
		t.Fatalf("pochi punti alla fine del bake: %+v", res)
	}
	// Non-critical metric failed: score 50 with weights 1 and 1, below marginal 75.
	in.Final = false
	in.Health.Metrics[0].Critical = false
	set("b", sameB)
	src.series[`latency{track="b"}`] = noisy(r, 20, 0.05)
	res = c.Check(ctx, in)
	if res.OK || res.Score == nil || *res.Score != 50 {
		t.Fatalf("metrica non critica fallita: %+v", res)
	}
	// With weight 4 on errors the score is 80: above marginal during the bake, below pass at the end.
	w := 4.0
	in.Health.Metrics[0].Weight = &w
	if res = c.Check(ctx, in); !res.OK || *res.Score != 80 {
		t.Fatalf("punteggio 80 durante il bake: %+v", res)
	}
	in.Final = true
	if res = c.Check(ctx, in); res.OK {
		t.Fatalf("punteggio 80 alla fine: atteso rosso (pass 95)")
	}
	// Without a stable version (first release) nothing to compare.
	in.StableTrack, in.Final = "", false
	if res = c.Check(ctx, in); !res.OK {
		t.Fatalf("primo rilascio: %s", res.Failed())
	}
}

func TestSorgentiHTTP(t *testing.T) {
	ctx := context.Background()
	secret := func(_ context.Context, ref string) (string, error) { return "sec-" + ref, nil }
	start, end := time.Unix(1700000000, 0), time.Unix(1700000300, 0)

	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/query_range":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1,"1.5"],[2,"NaN"],[3,"2"]]}]}}`))
		case r.URL.Path == "/api/v1/query" && r.Header.Get("Dd-Api-Key") != "":
			_, _ = w.Write([]byte(`{"status":"ok","series":[{"pointlist":[[1,0.1],[2,null],[3,0.3]]}]}`))
		case r.URL.Path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"actor":{"account":{"nrql":{"results":[{"beginTimeSeconds":1,"endTimeSeconds":2,"count":4},{"beginTimeSeconds":2,"endTimeSeconds":3,"count":6}]}}}}}`))
		case r.URL.Path == "/_query":
			_, _ = w.Write([]byte(`{"columns":[{"name":"bucket","type":"date"},{"name":"rate","type":"double"}],"values":[["t1",0.5],["t2",0.7]]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	prom := &PrometheusSource{URL: srv.URL, Client: srv.Client()}
	if v, err := prom.Series(ctx, "up", start, end, 15*time.Second); err != nil || len(v) != 2 || v[1] != 2 || got.URL.Query().Get("step") != "15" {
		t.Fatalf("prometheus: %v %v %s", v, err, got.URL.RawQuery)
	}

	dd := &DatadogSource{URL: srv.URL, APIKeyRef: "api", AppKeyRef: "app", Secret: secret, Client: srv.Client()}
	if v, err := dd.Value(ctx, "avg:errors{*}", end); err != nil || v != 0.3 {
		t.Fatalf("datadog: %v %v", v, err)
	}
	if got.Header.Get("Dd-Api-Key") != "sec-api" || got.Header.Get("Dd-Application-Key") != "sec-app" || got.URL.Query().Get("to") != "1700000300" {
		t.Errorf("datadog: richiesta %v %s", got.Header, got.URL.RawQuery)
	}

	nr := &NewRelicSource{URL: srv.URL + "/graphql", AccountID: 42, APIKeyRef: "nr", Secret: secret, Client: srv.Client()}
	v, err := nr.Series(ctx, "SELECT count(*) FROM Transaction", start, end, 30*time.Second)
	if err != nil || len(v) != 2 || v[1] != 6 || got.Header.Get("Api-Key") != "sec-nr" {
		t.Fatalf("newrelic: %v %v", v, err)
	}
	var gq struct {
		Variables struct {
			ID   int64  `json:"id"`
			NRQL string `json:"nrql"`
		} `json:"variables"`
	}
	_ = json.Unmarshal(body, &gq)
	if gq.Variables.ID != 42 || gq.Variables.NRQL != "SELECT count(*) FROM Transaction SINCE 1700000000000 UNTIL 1700000300000 TIMESERIES 30 seconds" {
		t.Errorf("newrelic: variabili %+v", gq.Variables)
	}

	es := &ElasticSource{URL: srv.URL, APIKeyRef: "es", Secret: secret, Client: srv.Client()}
	v, err = es.Series(ctx, `FROM logs | WHERE @timestamp >= "{start}" | STATS rate = AVG(x) BY bucket = BUCKET(@timestamp, {step})`, start, end, 15*time.Second)
	if err != nil || len(v) != 2 || v[0] != 0.5 || got.Header.Get("Authorization") != "ApiKey sec-es" {
		t.Fatalf("elastic: %v %v", v, err)
	}
	if !strings.Contains(string(body), `\"2023-11-14T22:13:20Z\"`) || !strings.Contains(string(body), "15 seconds") {
		t.Errorf("elastic: query %s", body)
	}
}
