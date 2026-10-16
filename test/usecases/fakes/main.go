// Command fakes emulates, for the use case lab, the external services manent talks to and that
// have no local version: the Slack Web API, Microsoft Teams webhooks, the ServiceNow Table API
// and a generic HTTP endpoint for custom steps. Every request is recorded and can be read on
// GET /records, so the cases check what manent sent. Never use it outside the lab.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
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
	seq     int
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
	default:
		reply(w, 404, map[string]string{"error": "percorso sconosciuto " + p})
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
		reply(w, 400, map[string]string{"error": "richiesta non valida"})
	}
}

// hook is the endpoint of the custom HTTP steps: POST /hooks/<name> starts a job, GET
// /hooks/<name>/jobs/<id> reports "running" the first time and "done" afterwards; a name
// starting with "fail" answers 500.
func (f *fakes) hook(w http.ResponseWriter, r *http.Request, body []byte) {
	f.add("hook", r, body)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/hooks/"), "/")
	if strings.HasPrefix(parts[0], "fail") {
		reply(w, 500, map[string]string{"error": "guasto simulato"})
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
	flag.Parse()
	f := &fakes{changes: map[string]map[string]any{}, polls: map[string]int{}}
	log.Printf("fakes in ascolto su %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, f))
}
