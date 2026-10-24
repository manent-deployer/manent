// Command fakes emulates, for the use case lab, the external services manent talks to and that
// have no local version: the Slack Web API, Microsoft Teams webhooks, the ServiceNow Table API,
// a generic HTTP endpoint for custom steps and the metric APIs of Datadog, New Relic
// (NerdGraph), Elastic (ES|QL) and CloudWatch (GetMetricData, Smithy RPC v2 CBOR). Every request is recorded and can be read on
// GET /records, so the cases check what manent sent. On another port it serves the emulated
// Azure of internal/azurefake (App Service, Container Apps, Kudu, Log Analytics). Never use it
// outside the lab.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/smithy-go/encoding/cbor"

	"github.com/joeg-ita/manent/internal/azurefake"
)

// record is a request received by the fakes.
type record struct {
	Seq     int               `json:"seq"`
	Kind    string            `json:"kind"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type fakes struct {
	mu      sync.Mutex
	records []record
	changes map[string]map[string]any // sys_id → ServiceNow change request
	polls   map[string]int            // job → GET count
	metrics []metricSeries            // series returned by the metric APIs
	seq     int
}

// metricSeries is the series a metric API returns for the queries of a provider that contain key.
type metricSeries struct {
	Provider string    `json:"provider"`
	Key      string    `json:"key"`
	Values   []float64 `json:"values"`
}

// series returns the values configured for the query (the most recent matching key).
func (f *fakes) series(provider, query string) []float64 {
	for i := len(f.metrics) - 1; i >= 0; i-- {
		if m := f.metrics[i]; m.Provider == provider && strings.Contains(query, m.Key) {
			return m.Values
		}
	}
	return nil
}

// recorded headers: authentication and idempotency, to check what manent sends.
var recorded = []string{"Authorization", "Idempotency-Key", "Content-Type", "X-Manent-Deployment"}

func (f *fakes) add(kind string, r *http.Request, body []byte) {
	f.seq++
	rec := record{Seq: f.seq, Kind: kind, Method: r.Method, Path: r.URL.RequestURI(), Headers: map[string]string{}}
	for _, h := range recorded {
		if v := r.Header.Get(h); v != "" {
			rec.Headers[h] = v
		}
	}
	if json.Valid(body) {
		rec.Body = body
	} else if len(body) > 0 {
		rec.Body, _ = json.Marshal(string(body))
	}
	f.records = append(f.records, rec)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/records":
		kind := r.URL.Query().Get("kind")
		out := []record{}
		for _, rec := range f.records {
			if kind == "" || rec.Kind == kind {
				out = append(out, rec)
			}
		}
		reply(w, 200, out)
	case strings.HasPrefix(p, "/slack/api/"):
		f.slack(w, r, body)
	case strings.HasPrefix(p, "/teams/"):
		f.add("teams", r, body)
		w.WriteHeader(http.StatusAccepted)
	case strings.HasPrefix(p, "/api/now/table/change_request"):
		f.servicenow(w, r, body)
	case strings.HasPrefix(p, "/control/servicenow/"):
		// POST /control/servicenow/<number>?approval=approved|rejected: the CAB decides.
		number := strings.TrimPrefix(p, "/control/servicenow/")
		for _, c := range f.changes {
			if c["number"] == number {
				c["approval"] = r.URL.Query().Get("approval")
				reply(w, 200, c)
				return
			}
		}
		reply(w, 404, map[string]string{"error": "change " + number + " inesistente"})
	case strings.HasPrefix(p, "/hooks/"):
		f.hook(w, r, body)
	case p == "/control/metrics":
		// POST /control/metrics {"provider":..., "key":..., "values":[...]}: series to return.
		var m metricSeries
		if err := json.Unmarshal(body, &m); err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		f.metrics = append(f.metrics, m)
		reply(w, 200, m)
	case strings.HasPrefix(p, "/datadog/"):
		f.datadog(w, r, body)
	case strings.HasPrefix(p, "/newrelic/"):
		f.newrelic(w, r, body)
	case strings.HasPrefix(p, "/elastic/"):
		f.elastic(w, r, body)
	case strings.HasPrefix(p, "/cloudwatch/"):
		f.cloudwatch(w, r, body)
	default:
		reply(w, 404, map[string]string{"error": "unknown path " + p})
	}
}

// slack emulates chat.postMessage and chat.update with the bot token xoxb-uc-test.
func (f *fakes) slack(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Header.Get("Authorization") != "Bearer xoxb-uc-test" {
		reply(w, 200, map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	f.add("slack", r, body)
	var msg map[string]any
	_ = json.Unmarshal(body, &msg)
	switch strings.TrimPrefix(r.URL.Path, "/slack/api/") {
	case "chat.postMessage":
		reply(w, 200, map[string]any{"ok": true, "channel": msg["channel"], "ts": fmt.Sprintf("%d.000100", f.seq)})
	case "chat.update":
		reply(w, 200, map[string]any{"ok": true, "channel": msg["channel"], "ts": msg["ts"]})
	default:
		reply(w, 200, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

// servicenow emulates the Table API on change_request with basic credentials manent/uc-snow.
// A new change waits for approval (approval=requested) until /control/servicenow decides.
func (f *fakes) servicenow(w http.ResponseWriter, r *http.Request, body []byte) {
	if u, pw, ok := r.BasicAuth(); !ok || u != "manent" || pw != "uc-snow" {
		reply(w, 401, map[string]any{"error": map[string]string{"message": "User Not Authenticated"}})
		return
	}
	f.add("servicenow", r, body)
	const base = "/api/now/table/change_request"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		q := r.URL.Query().Get("sysparm_query")
		out := []any{}
		for _, c := range f.changes {
			if strings.HasPrefix(q, "correlation_id="+fmt.Sprint(c["correlation_id"])+"^") {
				out = append(out, c)
			}
		}
		reply(w, 200, map[string]any{"result": out})
	case r.Method == http.MethodPost && r.URL.Path == base:
		var c map[string]any
		_ = json.Unmarshal(body, &c)
		n := len(f.changes) + 1
		id := fmt.Sprintf("sys%d", n)
		c["sys_id"], c["number"], c["state"], c["approval"] = id, fmt.Sprintf("CHG%07d", n), "-5", "requested"
		f.changes[id] = c
		reply(w, 201, map[string]any{"result": c})
	case strings.HasPrefix(r.URL.Path, base+"/"):
		c, ok := f.changes[strings.TrimPrefix(r.URL.Path, base+"/")]
		if !ok {
			reply(w, 404, map[string]any{"error": map[string]string{"message": "No Record found"}})
			return
		}
		if r.Method == http.MethodPatch {
			var patch map[string]any
			_ = json.Unmarshal(body, &patch)
			for k, v := range patch {
				c[k] = v
			}
		}
		reply(w, 200, map[string]any{"result": c})
	default:
		reply(w, 400, map[string]string{"error": "invalid request"})
	}
}

// hook is the endpoint of the custom HTTP steps: POST /hooks/<name> starts a job, GET
// /hooks/<name>/jobs/<id> reports "running" the first time and "done" afterwards; a name
// starting with "fail" answers 500.
func (f *fakes) hook(w http.ResponseWriter, r *http.Request, body []byte) {
	f.add("hook", r, body)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/hooks/"), "/")
	if strings.HasPrefix(parts[0], "fail") {
		reply(w, 500, map[string]string{"error": "simulated failure"})
		return
	}
	if r.Method == http.MethodPost {
		reply(w, 202, map[string]any{"job": fmt.Sprintf("job-%d", f.seq), "status": "running"})
		return
	}
	if len(parts) == 3 && parts[1] == "jobs" {
		f.polls[parts[2]]++
		status := "running"
		if f.polls[parts[2]] > 1 {
			status = "done"
		}
		reply(w, 200, map[string]any{"job": parts[2], "status": status, "ticket": "TCK-" + strings.TrimPrefix(parts[2], "job-")})
		return
	}
	reply(w, 200, map[string]string{"status": "ok"})
}

func main() {
	listen := flag.String("listen", ":9100", "listen address")
	azureListen := flag.String("azure", ":9400", "listen address of the emulated Azure (UC22)")
	flag.Parse()
	f := &fakes{changes: map[string]map[string]any{}, polls: map[string]int{}}
	go func() {
		log.Printf("emulated Azure listening on %s", *azureListen)
		log.Fatal(http.ListenAndServe(*azureListen, newAzure().Handler()))
	}()
	log.Printf("fakes listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, f))
}

// newAzure creates the emulated Azure of UC22 (internal/azurefake): web apps with and without
// slot, a container app in Multiple mode, the service principal with the lab secret.
func newAzure() *azurefake.Fake {
	az := azurefake.New("uc-azure-secret")
	az.AddSite("uc22", "uc22-web", "")
	az.AddSite("uc22", "uc22-web", "staging")
	az.AddSite("uc22", "uc22-ramp", "")
	az.AddSite("uc22", "uc22-ramp", "staging")
	az.AddSite("uc22", "uc22-site", "")
	az.AddApp("uc22", "uc22-api", "Multiple", "api", "registry:5000/uc/legacy:0")
	return az
}

// datadog emulates GET /api/v1/query with the keys uc-dd-api and uc-dd-app.
func (f *fakes) datadog(w http.ResponseWriter, r *http.Request, body []byte) {
	f.add("datadog", r, body)
	if r.Header.Get("Dd-Api-Key") != "uc-dd-api" || r.Header.Get("Dd-Application-Key") != "uc-dd-app" {
		reply(w, 403, map[string]any{"errors": []string{"Forbidden"}})
		return
	}
	var points [][]any
	for i, v := range f.series("datadog", r.URL.Query().Get("query")) {
		points = append(points, []any{float64(i) * 1000, v})
	}
	series := []any{}
	if len(points) > 0 {
		series = append(series, map[string]any{"pointlist": points})
	}
	reply(w, 200, map[string]any{"status": "ok", "series": series})
}

// newrelic emulates NerdGraph (POST /graphql) for NRQL queries with the key uc-nr-key.
func (f *fakes) newrelic(w http.ResponseWriter, r *http.Request, body []byte) {
	f.add("newrelic", r, body)
	if r.Header.Get("Api-Key") != "uc-nr-key" {
		reply(w, 200, map[string]any{"errors": []map[string]string{{"message": "Invalid API key"}}})
		return
	}
	var req struct {
		Variables struct {
			NRQL string `json:"nrql"`
		} `json:"variables"`
	}
	_ = json.Unmarshal(body, &req)
	vals := f.series("newrelic", req.Variables.NRQL)
	rows := []map[string]any{}
	if strings.Contains(req.Variables.NRQL, "TIMESERIES") {
		for i, v := range vals {
			rows = append(rows, map[string]any{"beginTimeSeconds": i, "endTimeSeconds": i + 1, "average.duration": v})
		}
	} else if len(vals) > 0 {
		rows = append(rows, map[string]any{"average.duration": vals[len(vals)-1]})
	}
	reply(w, 200, map[string]any{"data": map[string]any{"actor": map[string]any{"account": map[string]any{"nrql": map[string]any{"results": rows}}}}})
}

// elastic emulates ES|QL (POST /_query) with the API key uc-es-key.
func (f *fakes) elastic(w http.ResponseWriter, r *http.Request, body []byte) {
	f.add("elastic", r, body)
	if r.Header.Get("Authorization") != "ApiKey uc-es-key" {
		reply(w, 401, map[string]any{"error": map[string]string{"type": "security_exception"}})
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(body, &req)
	rows := [][]any{}
	for i, v := range f.series("elastic", req.Query) {
		rows = append(rows, []any{time.Unix(int64(i), 0).UTC().Format(time.RFC3339), v})
	}
	reply(w, 200, map[string]any{"columns": []map[string]string{{"name": "bucket", "type": "date"}, {"name": "value", "type": "double"}}, "values": rows})
}

// cloudwatch emulates GetMetricData with the Smithy RPC v2 CBOR protocol of the AWS SDK: it
// requires a SigV4 signature of the key AKIDUCTEST and returns, for each query, the series
// configured for its expression.
func (f *fakes) cloudwatch(w http.ResponseWriter, r *http.Request, body []byte) {
	in, err := cbor.Decode(body)
	var expressions []string
	if m, ok := in.(cbor.Map); ok && err == nil {
		if qs, ok := m["MetricDataQueries"].(cbor.List); ok {
			for _, q := range qs {
				if qm, ok := q.(cbor.Map); ok {
					if e, ok := qm["Expression"].(cbor.String); ok {
						expressions = append(expressions, string(e))
					}
				}
			}
		}
	}
	rec, _ := json.Marshal(map[string]any{"operation": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "expressions": expressions})
	f.add("cloudwatch", r, rec)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDUCTEST/") {
		w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(403)
		_, _ = w.Write(cbor.Encode(cbor.Map{"__type": cbor.String("AccessDeniedException"), "message": cbor.String("missing signature")}))
		return
	}
	results := cbor.List{}
	for i, e := range expressions {
		vals, ts := cbor.List{}, cbor.List{}
		for j, v := range f.series("cloudwatch", e) {
			vals = append(vals, cbor.Float64(v))
			ts = append(ts, &cbor.Tag{ID: 1, Value: cbor.Float64(float64(1700000000 + 60*j))})
		}
		results = append(results, cbor.Map{"Id": cbor.String(fmt.Sprintf("m%d", i)), "Values": vals, "Timestamps": ts, "StatusCode": cbor.String("Complete")})
	}
	w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
	w.Header().Set("Content-Type", "application/cbor")
	_, _ = io.Copy(w, bytes.NewReader(cbor.Encode(cbor.Map{"MetricDataResults": results})))
}
