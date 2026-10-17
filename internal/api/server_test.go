package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/authz"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/telemetry"
)

// mockService implements engine.Service for the handler tests.
type mockService struct {
	submitted *descriptor.Descriptor
	filter    engine.ListFilter
	op        string
	onSubmit  func(context.Context)
}

func (m *mockService) Plan(context.Context, *descriptor.Descriptor) (pipeline.Plan, error) {
	return pipeline.Plan{{Name: "deploy"}}, nil
}

func (m *mockService) Submit(ctx context.Context, d *descriptor.Descriptor) (*engine.Submission, error) {
	m.submitted = d
	if m.onSubmit != nil {
		m.onSubmit(ctx)
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		return &engine.Submission{State: engine.StateScheduled, Schedule: d.ScheduleName()}, nil
	}
	return &engine.Submission{ID: "dep-1", State: "ENQUEUED"}, nil
}

func (m *mockService) List(_ context.Context, f engine.ListFilter) ([]engine.Status, error) {
	m.filter = f
	return []engine.Status{{ID: "dep-1"}}, nil
}

func (m *mockService) Approvals(context.Context) ([]engine.Status, error) {
	return []engine.Status{{ID: "dep-2", State: engine.StateAwaitingApproval}}, nil
}

func (m *mockService) Workers(context.Context) ([]engine.WorkerInfo, error) {
	return []engine.WorkerInfo{{ID: "w1", State: "alive", Tasks: []engine.WorkerTask{{ID: "dep-1", Kind: "deploy"}}}}, nil
}

func (m *mockService) Activity(_ context.Context, f engine.ActivityFilter) (*engine.Activity, error) {
	m.op = "activity:" + f.Application + ":" + f.Environment
	return &engine.Activity{Running: []engine.Status{{ID: "dep-1", State: "PENDING", Executor: "w1", LastStep: "deploy"}}}, nil
}

func (m *mockService) SubmitRelease(_ context.Context, r *descriptor.Release) (*engine.ReleaseStatus, error) {
	m.op = "release:" + r.ID
	return &engine.ReleaseStatus{ID: r.ID, State: "PENDING"}, nil
}

func (m *mockService) ReleaseStatus(_ context.Context, id string) (*engine.ReleaseStatus, error) {
	if id != "r1" {
		return nil, engine.ErrNotFound
	}
	return &engine.ReleaseStatus{ID: id, State: "SUCCESS"}, nil
}

func (m *mockService) ListReleases(_ context.Context, limit int) ([]engine.ReleaseStatus, error) {
	m.op = "releases:" + strconv.Itoa(limit)
	return []engine.ReleaseStatus{{ID: "r1"}}, nil
}

func (m *mockService) CancelRelease(_ context.Context, id string) error {
	m.op = "cancel-release:" + id
	return nil
}

func (m *mockService) Me(ctx context.Context) (*engine.Identity, error) {
	p := auth.FromContext(ctx)
	return &engine.Identity{Subject: p.String(), Name: p.Subject, Method: p.Method}, nil
}

func (m *mockService) Cancel(_ context.Context, id string) error {
	if id != "dep-1" {
		return engine.ErrNotFound
	}
	return nil
}

func (m *mockService) ListSchedules(context.Context) ([]engine.ScheduleInfo, error) {
	return []engine.ScheduleInfo{{Name: "nightly"}}, nil
}

func (m *mockService) GetSchedule(_ context.Context, name string) (*engine.ScheduleInfo, error) {
	if name != "nightly" {
		return nil, engine.ErrNotFound
	}
	return &engine.ScheduleInfo{Name: name}, nil
}

func (m *mockService) PauseSchedule(_ context.Context, name string) error {
	m.op = "pause:" + name
	return nil
}

func (m *mockService) ResumeSchedule(_ context.Context, name string) error {
	m.op = "resume:" + name
	return nil
}

func (m *mockService) DeleteSchedule(_ context.Context, name string) error {
	m.op = "delete:" + name
	return nil
}

func (m *mockService) Status(_ context.Context, id string) (*engine.Status, error) {
	if id != "dep-1" {
		return nil, engine.ErrNotFound
	}
	return &engine.Status{ID: id, State: "SUCCESS"}, nil
}

func (m *mockService) Wait(context.Context, string) (*engine.Result, error) { return nil, nil }

func (m *mockService) Inventory(_ context.Context, f engine.InventoryFilter) ([]engine.InventoryEntry, error) {
	m.op = "inventory:" + f.Application + "/" + f.Environment
	return []engine.InventoryEntry{{Application: "app", Environment: "prod"}}, nil
}

func (m *mockService) Promote(_ context.Context, r engine.PromoteRequest) (*engine.Submission, error) {
	m.op = "promote:" + r.Application + ":" + r.From + "->" + r.To
	return &engine.Submission{ID: "promo-1", State: engine.StateAwaitingApproval}, nil
}

func (m *mockService) Rollback(_ context.Context, r engine.RollbackRequest) (*engine.Submission, error) {
	if r.Version == "0.0.0" {
		return nil, engine.ErrInvalidRequest
	}
	m.op = "rollback:" + r.Application + "/" + r.Environment + "@" + r.Version
	return &engine.Submission{ID: "rb-1", State: "ENQUEUED"}, nil
}

func (m *mockService) Decide(_ context.Context, id string, r engine.DecisionRequest) error {
	if id != "dep-1" {
		return engine.ErrNotFound
	}
	m.op = "decide:" + strconv.FormatBool(r.Approve) + ":" + r.Comment
	return nil
}

func (m *mockService) Freezes(context.Context, string, string) ([]governance.WindowStatus, error) {
	return []governance.WindowStatus{{Name: "natale", Active: true}}, nil
}

func (m *mockService) ValidateEnvironment(_ context.Context, c engine.EnvironmentCheck) (*engine.Validation, error) {
	return &engine.Validation{Valid: true, Config: map[string]string{"source": "configurazione candidata", "application": c.Application}}, nil
}

func (m *mockService) Validate(_ context.Context, d *descriptor.Descriptor) (*engine.Validation, error) {
	if d.Environment == "prod" {
		return nil, fmt.Errorf("%w: policy", engine.ErrInvalidRequest)
	}
	return &engine.Validation{Valid: true, Plan: pipeline.Plan{{Name: "deploy"}}, Secrets: []string{"key"}}, nil
}

func (m *mockService) Steps(_ context.Context, id string) ([]engine.StepRecord, error) {
	if id != "dep-1" {
		return nil, engine.ErrNotFound
	}
	return []engine.StepRecord{{ID: 0, Name: "config"}, {ID: 1, Name: "deploy", Error: "boom"}}, nil
}

func (m *mockService) Logs(_ context.Context, _ string, offset int) (*deploylog.Page, error) {
	lines := []deploylog.Line{{Message: "uno"}, {Message: "due"}}[offset:]
	return &deploylog.Page{Lines: lines, Next: 2, Done: true}, nil
}

func (m *mockService) DORA(_ context.Context, f engine.DORAFilter) (*engine.DORAReport, error) {
	m.op = "dora:" + f.Application + ":" + f.Since.Format("2006-01-02")
	return &engine.DORAReport{Total: engine.DORAMetrics{Deployments: 3}}, nil
}

const validJSON = `{
  "apiVersion": "manent/v1",
  "application": "app",
  "environment": "test",
  "artifact": {"type": "application", "name": "app", "version": "1.0.0", "source": "/tmp/app.tar.gz"},
  "deploy": {"targets": [{"host": "h1", "user": "deploy"}]}
}`

func newTestRouter(svc engine.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	return NewRouter(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestSubmit(t *testing.T) {
	svc := &mockService{}
	w := do(newTestRouter(svc), http.MethodPost, "/api/v1/deployments", validJSON)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "dep-1" || svc.submitted.Application != "app" {
		t.Errorf("risposta inattesa: %v", resp)
	}
}

func TestSubmitDescrittoreNonValido(t *testing.T) {
	w := do(newTestRouter(&mockService{}), http.MethodPost, "/api/v1/deployments", `{"apiVersion":"manent/v1","campo":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("atteso 400, ricevuto %d", w.Code)
	}
}

func TestStatus(t *testing.T) {
	r := newTestRouter(&mockService{})
	if w := do(r, http.MethodGet, "/api/v1/deployments/dep-1", ""); w.Code != http.StatusOK {
		t.Errorf("atteso 200, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments/altro", ""); w.Code != http.StatusNotFound {
		t.Errorf("atteso 404, ricevuto %d", w.Code)
	}
}

func TestSubmitScheduleRicorrente(t *testing.T) {
	body := strings.Replace(validJSON, `"deploy":`, `"schedule": {"cron": "0 2 * * *", "name": "nightly"}, "deploy":`, 1)
	w := do(newTestRouter(&mockService{}), http.MethodPost, "/api/v1/deployments", body)
	if w.Code != http.StatusCreated || w.Header().Get("Location") != "/api/v1/schedules/nightly" {
		t.Fatalf("status %d, location %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
}

func TestListECancel(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodGet, "/api/v1/deployments?schedule=nightly&limit=5", ""); w.Code != http.StatusOK {
		t.Fatalf("list: status %d", w.Code)
	}
	if svc.filter.Schedule != "nightly" || svc.filter.Limit != 5 {
		t.Errorf("filtro inatteso: %+v", svc.filter)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments?limit=x", ""); w.Code != http.StatusBadRequest {
		t.Errorf("limit non valido: atteso 400, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/dep-1/cancel", ""); w.Code != http.StatusNoContent {
		t.Errorf("cancel: atteso 204, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/altro/cancel", ""); w.Code != http.StatusNotFound {
		t.Errorf("cancel inesistente: atteso 404, ricevuto %d", w.Code)
	}
}

func TestSchedules(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodGet, "/api/v1/schedules", ""); w.Code != http.StatusOK {
		t.Errorf("list: atteso 200, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/schedules/altra", ""); w.Code != http.StatusNotFound {
		t.Errorf("get inesistente: atteso 404, ricevuto %d", w.Code)
	}
	for _, tc := range []struct{ method, path, op string }{
		{http.MethodPost, "/api/v1/schedules/nightly/pause", "pause:nightly"},
		{http.MethodPost, "/api/v1/schedules/nightly/resume", "resume:nightly"},
		{http.MethodDelete, "/api/v1/schedules/nightly", "delete:nightly"},
	} {
		if w := do(r, tc.method, tc.path, ""); w.Code != http.StatusNoContent || svc.op != tc.op {
			t.Errorf("%s %s: status %d, op %q", tc.method, tc.path, w.Code, svc.op)
		}
	}
}

func TestSubmitDryRun(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodPost, "/api/v1/deployments?dry_run=true", validJSON); w.Code != http.StatusAccepted || !svc.submitted.DryRun {
		t.Fatalf("status %d, dryRun %v: %s", w.Code, svc.submitted.DryRun, w.Body)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments?dry_run=forse", validJSON); w.Code != http.StatusBadRequest {
		t.Errorf("atteso 400, ricevuto %d", w.Code)
	}
}

func TestAutenticazioneEPermessi(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sum := sha256.Sum256([]byte("ci-token"))
	tokens := auth.Tokens{{Name: "ci", SHA256: hex.EncodeToString(sum[:])}}
	pol, err := authz.NewPolicy(nil, []authz.Binding{
		{Subjects: []string{"token:ci"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var seen *auth.Principal
	svc := &mockService{onSubmit: func(ctx context.Context) { seen = auth.FromContext(ctx) }}
	guard := &authz.Guard{Next: svc, Policy: pol, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := NewRouter(guard, tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := func(token, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		hr := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body))
		if token != "" {
			hr.Header.Set("Authorization", "Bearer "+token)
		}
		r.ServeHTTP(w, hr)
		return w
	}
	if w := req("", validJSON); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("atteso 401 senza token, ricevuto %d", w.Code)
	}
	if w := req("sbagliato", validJSON); w.Code != http.StatusUnauthorized {
		t.Fatalf("atteso 401 con token errato, ricevuto %d", w.Code)
	}
	if w := req("ci-token", validJSON); w.Code != http.StatusAccepted {
		t.Fatalf("atteso 202, ricevuto %d: %s", w.Code, w.Body)
	}
	if seen.String() != "token:ci" {
		t.Errorf("identità non propagata al servizio: %v", seen)
	}
	prod := strings.Replace(validJSON, `"environment": "test"`, `"environment": "prod"`, 1)
	if w := req("ci-token", prod); w.Code != http.StatusForbidden {
		t.Fatalf("atteso 403 in prod, ricevuto %d: %s", w.Code, w.Body)
	}
	// /healthz needs no authentication.
	if w := do(r, http.MethodGet, "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestGovernance(t *testing.T) {
	svc := &mockService{}
	r := newTestRouter(svc)
	if w := do(r, http.MethodPost, "/api/v1/deployments/dep-1/approve", ""); w.Code != http.StatusNoContent || svc.op != "decide:true:" {
		t.Errorf("approve senza body: status %d, op %q", w.Code, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/dep-1/reject", `{"comment":"no"}`); w.Code != http.StatusNoContent || svc.op != "decide:false:no" {
		t.Errorf("reject: status %d, op %q", w.Code, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/deployments/altro/approve", ""); w.Code != http.StatusNotFound {
		t.Errorf("approve inesistente: atteso 404, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/promotions", `{"application":"app","from":"test","to":"prod"}`); w.Code != http.StatusAccepted || svc.op != "promote:app:test->prod" {
		t.Errorf("promotions: status %d, op %q", w.Code, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/promotions", `{"application":"app","campo":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("promotions con campo sconosciuto: atteso 400, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodPost, "/api/v1/rollbacks", `{"application":"app","environment":"prod","version":"1.0.0"}`); w.Code != http.StatusAccepted || svc.op != "rollback:app/prod@1.0.0" {
		t.Errorf("rollbacks: status %d, op %q", w.Code, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/rollbacks", `{"application":"app","environment":"prod","version":"0.0.0"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("rollback non ammesso: atteso 422, ricevuto %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/inventory?application=app&environment=prod", ""); w.Code != http.StatusOK || svc.op != "inventory:app/prod" {
		t.Errorf("inventory: status %d, op %q", w.Code, svc.op)
	}
	if w := do(r, http.MethodGet, "/api/v1/freezes", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "natale") {
		t.Errorf("freezes: status %d: %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments?application=app&environment=prod&state=SUCCESS", ""); w.Code != http.StatusOK ||
		svc.filter.Application != "app" || svc.filter.Environment != "prod" || svc.filter.State != "SUCCESS" {
		t.Errorf("filtri dell'elenco inattesi: %+v", svc.filter)
	}
}

func TestOsservabilita(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockService{}
	metrics := telemetry.NewMetrics(telemetry.BuildInfo{Version: "1.2.3"})
	ready := errors.New("database irraggiungibile")
	r := NewRouterWith(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Metrics: metrics, MetricsHandler: metrics.Handler(),
		Ready: func(context.Context) error { return ready },
	})

	if w := do(r, http.MethodGet, "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz con database irraggiungibile: %d", w.Code)
	}
	ready = nil
	if w := do(r, http.MethodGet, "/readyz", ""); w.Code != http.StatusOK {
		t.Errorf("readyz: %d", w.Code)
	}

	w := do(r, http.MethodGet, "/api/v1/deployments/dep-1/steps", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error":"boom"`) {
		t.Errorf("steps: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments/altro/steps", ""); w.Code != http.StatusNotFound {
		t.Errorf("steps di un deploy inesistente: %d", w.Code)
	}
	w = do(r, http.MethodGet, "/api/v1/deployments/dep-1/logs?offset=1", "")
	var p deploylog.Page
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || len(p.Lines) != 1 || p.Lines[0].Message != "due" || !p.Done {
		t.Errorf("logs: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/api/v1/deployments/dep-1/logs?offset=-1", ""); w.Code != http.StatusBadRequest {
		t.Errorf("offset negativo: %d", w.Code)
	}
	w = do(r, http.MethodGet, "/api/v1/metrics/dora?application=app&since=2026-09-01T00:00:00Z", "")
	if w.Code != http.StatusOK || svc.op != "dora:app:2026-09-01" {
		t.Errorf("dora: %d %s (%s)", w.Code, w.Body, svc.op)
	}
	if w := do(r, http.MethodGet, "/api/v1/metrics/dora?since=ieri", ""); w.Code != http.StatusBadRequest {
		t.Errorf("since non valido: %d", w.Code)
	}

	w = do(r, http.MethodGet, "/api/v1/workers", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workers":[{"id":"w1","state":"alive"`) {
		t.Errorf("workers: %d %s", w.Code, w.Body)
	}
	w = do(r, http.MethodGet, "/api/v1/activity?application=app&environment=prod", "")
	if w.Code != http.StatusOK || svc.op != "activity:app:prod" || !strings.Contains(w.Body.String(), `"last_step":"deploy"`) {
		t.Errorf("activity: %d %s (%s)", w.Code, w.Body, svc.op)
	}

	rel := `{"apiVersion":"manent/v1","kind":"Release","id":"r1","deploys":[{"name":"api","descriptor":` + validJSON + `}]}`
	if w := do(r, http.MethodPost, "/api/v1/releases", rel); w.Code != http.StatusAccepted || svc.op != "release:r1" {
		t.Errorf("submit release: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodPost, "/api/v1/releases", `{"apiVersion":"manent/v1","kind":"Release","id":"r1","deploys":[]}`); w.Code != http.StatusBadRequest {
		t.Errorf("release senza deploy: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/releases/r1", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"SUCCESS"`) {
		t.Errorf("release: %d %s", w.Code, w.Body)
	}
	if w := do(r, http.MethodGet, "/api/v1/releases/altra", ""); w.Code != http.StatusNotFound {
		t.Errorf("release inesistente: %d", w.Code)
	}
	if w := do(r, http.MethodGet, "/api/v1/releases?limit=5", ""); w.Code != http.StatusOK || svc.op != "releases:5" {
		t.Errorf("elenco release: %d %s", w.Code, svc.op)
	}
	if w := do(r, http.MethodPost, "/api/v1/releases/r1/cancel", ""); w.Code != http.StatusNoContent || svc.op != "cancel-release:r1" {
		t.Errorf("annullamento release: %d %s", w.Code, svc.op)
	}

	w = do(r, http.MethodGet, "/metrics", "")
	body := w.Body.String()
	for _, want := range []string{
		`manent_build_info{commit="",version="1.2.3",workflow_version=""} 1`,
		`manent_http_request_duration_seconds_count{code="200",method="GET",route="/api/v1/deployments/:id/steps"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrica mancante %q", want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"30d":                  now.AddDate(0, 0, -30),
		"48h":                  now.Add(-48 * time.Hour),
		"2026-01-01T00:00:00Z": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		if got, err := service.ParseSince(in, now); err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-1h", "domani"} {
		if _, err := service.ParseSince(in, now); err == nil {
			t.Errorf("%q accettato", in)
		}
	}
}

func TestValidate(t *testing.T) {
	r := newTestRouter(&mockService{})
	w := do(r, http.MethodPost, "/api/v1/deployments/validate", validJSON)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"secrets":["key"]`) {
		t.Fatalf("validate: %d %s", w.Code, w.Body)
	}
	prod := strings.Replace(validJSON, `"environment": "test"`, `"environment": "prod"`, 1)
	if w := do(r, http.MethodPost, "/api/v1/deployments/validate", prod); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validate non valido: %d %s", w.Code, w.Body)
	}
}
