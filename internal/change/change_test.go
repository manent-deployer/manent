package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type mapSecrets map[string]string

func (m mapSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("segreto %s assente", ref)
}

func request() Request {
	return Request{
		DeploymentID: "shop-1.4.0-prod", Application: "shop", Environment: "prod", Artifact: "shop", Version: "1.4.0",
		Kind: "promotion", RequestedBy: "oidc:alice", URL: "https://manent.example.com/deployments/shop-1.4.0-prod", Approval: true,
	}
}

// fakeServiceNow emulates the Table API on change_request.
type fakeServiceNow struct {
	mu      sync.Mutex
	records map[string]map[string]any
	patches []map[string]any
	creates int
}

func (f *fakeServiceNow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != "manent" || p != "pw" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"User Not Authenticated"}}`))
		return
	}
	const base = "/api/now/table/change_request"
	reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"result": v}) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		q := r.URL.Query().Get("sysparm_query")
		var out []any
		for _, rec := range f.records {
			if strings.HasPrefix(q, "correlation_id="+fmt.Sprint(rec["correlation_id"])+"^") {
				out = append(out, rec)
			}
		}
		reply(out)
	case r.Method == http.MethodPost && r.URL.Path == base:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.creates++
		id := fmt.Sprintf("sys%d", f.creates)
		body["sys_id"], body["number"], body["state"], body["approval"] = id, fmt.Sprintf("CHG%07d", f.creates), "-5", "requested"
		f.records[id] = body
		w.WriteHeader(http.StatusCreated)
		reply(body)
	case strings.HasPrefix(r.URL.Path, base+"/"):
		rec, ok := f.records[strings.TrimPrefix(r.URL.Path, base+"/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPatch {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["state"] == "4" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.patches = append(f.patches, body)
			for k, v := range body {
				rec[k] = v
			}
		}
		reply(rec)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestServiceNow(t *testing.T) {
	f := &fakeServiceNow{records: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	sn := &ServiceNow{
		URL: srv.URL, Username: "manent", PasswordRef: "pw-ref", Secrets: mapSecrets{"pw-ref": "pw\n"}, Client: srv.Client(),
		Fields:     map[string]string{"cmdb_ci": "{application}-{environment}", "assignment_group": "CAB", "correlation_id": "other"},
		StartState: SNStateImplement, CloseStates: []string{SNStateReview, SNStateClosed},
		CloseCodes: map[string]string{"success": "successful", "failure": "unsuccessful"},
	}
	ctx := context.Background()
	t1, err := sn.Open(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if t1.Number != "CHG0000001" || t1.Approval != ApprovalPending || !strings.Contains(t1.URL, "change_request.do%3Fsys_id%3Dsys1") {
		t.Fatalf("ticket: %+v", t1)
	}
	rec := f.records["sys1"]
	if rec["correlation_id"] != "shop-1.4.0-prod" || rec["cmdb_ci"] != "shop-prod" || rec["assignment_group"] != "CAB" ||
		!strings.Contains(rec["short_description"].(string), "Promotion shop 1.4.0 to prod") || !strings.Contains(rec["description"].(string), "oidc:alice") {
		t.Errorf("record creato: %v", rec)
	}
	// Open is idempotent: it finds the deploy's ticket again.
	if t2, err := sn.Open(ctx, request()); err != nil || t2.ID != "sys1" || f.creates != 1 {
		t.Fatalf("seconda apertura: %+v, %v (%d creazioni)", t2, err, f.creates)
	}
	f.records["sys1"]["approval"] = "approved"
	if t3, _ := sn.Refresh(ctx, t1); t3.Approval != ApprovalApproved {
		t.Errorf("approvazione: %+v", t3)
	}
	if err := sn.Start(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := sn.Close(ctx, t1, Outcome{Success: false, Message: "timeout", Rollback: "completed"}); err != nil {
		t.Fatal(err)
	}
	// A repeated close does not reset states already reached.
	if err := sn.Close(ctx, t1, Outcome{Success: false}); err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 3 || f.patches[0]["state"] != "-1" || f.patches[1]["state"] != "0" || f.patches[2]["state"] != "3" ||
		f.patches[2]["close_code"] != "unsuccessful" || !strings.Contains(f.patches[1]["close_notes"].(string), "Automatic rollback: completed") {
		t.Errorf("aggiornamenti: %v", f.patches)
	}

	f.records["sys1"]["state"], f.records["sys1"]["approval"] = "4", "requested"
	if t4, _ := sn.Refresh(ctx, t1); t4.Approval != ApprovalRejected {
		t.Errorf("change annullato: %+v", t4)
	}
	sn.Secrets = mapSecrets{"pw-ref": "sbagliata"}
	if _, err := sn.Open(ctx, request()); !errors.Is(err, ErrPermanent) {
		t.Errorf("credenziali errate: %v", err)
	}
}

// fakeJira emulates the Jira REST API v2; with cloud=false it has no /search/jql (Data Center).
type fakeJira struct {
	mu          sync.Mutex
	cloud       bool
	issues      map[string]map[string]any
	comments    []string
	transitions []string
}

func (f *fakeJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer pat" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	issue := func(key string) map[string]any {
		i := f.issues[key]
		return map[string]any{"id": i["id"], "key": key, "fields": map[string]any{"status": map[string]any{"name": i["status"]}}}
	}
	p := strings.TrimPrefix(r.URL.Path, "/rest/api/2")
	switch {
	case p == "/search/jql" && !f.cloud:
		w.WriteHeader(http.StatusNotFound)
	case p == "/search" || p == "/search/jql":
		var out []any
		for key, i := range f.issues {
			if strings.Contains(r.URL.Query().Get("jql"), `labels = "`+i["label"].(string)+`"`) {
				out = append(out, issue(key))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": out})
	case p == "/issue" && r.Method == http.MethodPost:
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := fmt.Sprintf("CHG-%d", len(f.issues)+1)
		labels := body.Fields["labels"].([]any)
		f.issues[key] = map[string]any{"id": fmt.Sprint(100 + len(f.issues)), "status": "Awaiting approval", "label": labels[1], "fields": body.Fields}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.issues[key]["id"], "key": key})
	case strings.HasSuffix(p, "/transitions") && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"transitions": []any{
			map[string]any{"id": "11", "name": "Implement", "to": map[string]any{"name": "Implementing"}},
			map[string]any{"id": "21", "name": "Done", "to": map[string]any{"name": "Done"}},
		}})
	case strings.HasSuffix(p, "/transitions"):
		var body map[string]map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.transitions = append(f.transitions, body["transition"]["id"])
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(p, "/comment"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.comments = append(f.comments, body["body"])
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	case strings.HasPrefix(p, "/issue/"):
		_ = json.NewEncoder(w).Encode(issue(strings.TrimPrefix(p, "/issue/")))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestJira(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		t.Run(fmt.Sprintf("cloud=%v", cloud), func(t *testing.T) {
			f := &fakeJira{cloud: cloud, issues: map[string]map[string]any{}}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			j := &Jira{
				URL: srv.URL, TokenRef: "tok", Secrets: mapSecrets{"tok": "pat"}, Client: srv.Client(),
				Project: "CHG", IssueType: "Change", Fields: map[string]any{"components": []any{map[string]any{"name": "{application}"}}, "labels": []any{"release", "{environment}", "manent"}},
				ApprovedStatuses: []string{"approved"}, RejectedStatuses: []string{"Declined"},
				Transitions: map[string]string{"start": "Implement", "success": "Done", "failure": "Failed"},
			}
			ctx := context.Background()
			t1, err := j.Open(ctx, request())
			if err != nil {
				t.Fatal(err)
			}
			if t1.Number != "CHG-1" || t1.Approval != ApprovalPending || t1.URL != srv.URL+"/browse/CHG-1" {
				t.Fatalf("issue: %+v", t1)
			}
			fields := f.issues["CHG-1"]["fields"].(map[string]any)
			if fmt.Sprint(fields["components"]) != "[map[name:shop]]" || fmt.Sprint(fields["labels"]) != "[manent manent-shop-1.4.0-prod release prod]" {
				t.Errorf("campi: %v", fields)
			}
			if t2, _ := j.Open(ctx, request()); t2.Number != "CHG-1" || len(f.issues) != 1 {
				t.Fatalf("seconda apertura: %+v", t2)
			}
			f.issues["CHG-1"]["status"] = "Approved"
			if t3, _ := j.Refresh(ctx, t1); t3.Approval != ApprovalApproved {
				t.Errorf("approvazione: %+v", t3)
			}
			if err := j.Start(ctx, t1); err != nil {
				t.Fatal(err)
			}
			if err := j.Close(ctx, t1, Outcome{Success: true}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(f.transitions) != "[11 21]" || len(f.comments) != 1 || !strings.Contains(f.comments[0], "completed successfully") {
				t.Errorf("transizioni %v, commenti %v", f.transitions, f.comments)
			}
			// The failure transition does not exist: permanent error.
			if err := j.Close(ctx, t1, Outcome{Success: false, Message: "x"}); !errors.Is(err, ErrPermanent) {
				t.Errorf("transizione assente: %v", err)
			}
		})
	}
	if l := Label("shop 1.4/prod"); l != "manent-shop-1.4-prod" {
		t.Errorf("etichetta: %s", l)
	}
}
