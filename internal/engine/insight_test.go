package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/telemetry"
	"github.com/joeg-ita/manent/internal/testdb"
)

// versionStep fails for version "bad" and writes to the deploy log.
type versionStep struct{ log *slog.Logger }

func (versionStep) Name() string { return "work" }

func (s versionStep) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	s.log.InfoContext(ctx, "copia sul target", "host", "h1", "version", in.Descriptor.Artifact.Version)
	s.log.DebugContext(ctx, "dettaglio non riportato")
	// A derived context (e.g. with a timeout) stays bound to the deploy.
	sub, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	s.log.WarnContext(sub, "target lento")
	if in.Descriptor.Artifact.Version == "bad" {
		return nil, pipeline.Permanent(errors.New("versione difettosa"))
	}
	return pipeline.Output{"ok": "true"}, nil
}

func release(id, env, version string) *descriptor.Descriptor {
	d := testDescriptor(id)
	d.Environment = env
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: version, Source: "/dev/null"}
	return d
}

func newInsightEngine(t *testing.T, metrics telemetry.Recorder) (*Engine, *Worker) {
	t.Helper()
	// The application log is at warn level: the deploy log still collects from INFO.
	log := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
	reg, planner := testRegistry(t, versionStep{log: log})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Metrics = metrics
	w := startWorker(t, opts)
	return NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard}), w
}

// run enqueues a deploy and waits for it to end, ignoring the outcome.
func run(t *testing.T, e *Engine, d *descriptor.Descriptor) {
	t.Helper()
	ctx := context.Background()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.Wait(ctx, sub.ID)
}

func TestLogETimelineDelDeploy(t *testing.T) {
	e, _ := newInsightEngine(t, nil)
	ctx := context.Background()
	run(t, e, release("log-1", "test", "1.0.0"))

	p, err := e.Logs(ctx, "log-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 2 || !p.Done || p.Next != 2 {
		t.Fatalf("log inatteso: %+v", p)
	}
	l := p.Lines[0]
	if l.Step != "work" || l.Message != "copia sul target" || l.Level != "INFO" || l.Attrs["host"] != "h1" || l.Attrs["version"] != "1.0.0" {
		t.Errorf("riga inattesa: %+v", l)
	}
	if p.Lines[1].Message != "target lento" {
		t.Errorf("riga da context derivato mancante: %+v", p.Lines[1])
	}
	if p, _ := e.Logs(ctx, "log-1", 1); len(p.Lines) != 1 {
		t.Errorf("offset non applicato: %+v", p)
	}

	steps, err := e.Steps(ctx, "log-1")
	if err != nil {
		t.Fatal(err)
	}
	var work *StepRecord
	for i := range steps {
		if steps[i].Name == "work" {
			work = &steps[i]
		}
	}
	if work == nil || work.StartedAt == nil || work.CompletedAt == nil || work.Error != "" {
		t.Fatalf("step work non registrato: %+v", steps)
	}
	if _, err := e.Steps(ctx, "inesistente"); !errors.Is(err, ErrNotFound) {
		t.Errorf("atteso ErrNotFound, ricevuto %v", err)
	}
	if _, err := e.Logs(ctx, "inesistente", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("atteso ErrNotFound, ricevuto %v", err)
	}
}

func TestMetricheDelWorker(t *testing.T) {
	m := telemetry.NewMetrics(telemetry.BuildInfo{Version: "test"})
	e, _ := newInsightEngine(t, m)
	run(t, e, release("m-1", "test", "1.0.0"))
	run(t, e, release("m-2", "test", "bad"))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`manent_deployments_total{application="app",dry_run="false",environment="test",kind="deploy",outcome="succeeded"} 1`,
		`manent_deployments_total{application="app",dry_run="false",environment="test",kind="deploy",outcome="failed"} 1`,
		`manent_step_duration_seconds_count{outcome="failed",step="work"} 1`,
		`manent_deployment_duration_seconds_count{application="app",environment="test",outcome="succeeded"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrica mancante: %s", want)
		}
	}
}

func TestDORA(t *testing.T) {
	e, _ := newInsightEngine(t, nil)
	ctx := context.Background()
	run(t, e, release("t1", "test", "1.0.0"))
	run(t, e, release("p1", "prod", "1.0.0"))
	run(t, e, release("p2", "prod", "bad"))
	run(t, e, release("p3", "prod", "1.1.0"))
	dry := release("dry", "prod", "bad")
	dry.DryRun = true
	run(t, e, dry)

	rep, err := e.DORA(ctx, DORAFilter{Application: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Environments) != 2 {
		t.Fatalf("ambienti inattesi: %+v", rep.Environments)
	}
	prod := rep.Environments[0]
	if prod.Environment != "prod" || prod.Deployments != 3 || prod.Failures != 1 || prod.ChangeFailureRate != 0.33 {
		t.Errorf("metriche di prod inattese: %+v", prod)
	}
	if prod.LeadTimeSeconds < 0 || rep.Total.Deployments != 4 || rep.Total.Failures != 1 {
		t.Errorf("totale inatteso: %+v", rep.Total)
	}
	if rep.Total.DeploymentsPerDay != 0.1 {
		t.Errorf("frequenza inattesa (3 riusciti in 30 giorni): %v", rep.Total.DeploymentsPerDay)
	}
	// The period excludes earlier deploys.
	rep, _ = e.DORA(ctx, DORAFilter{Since: time.Now().Add(time.Hour)})
	if rep.Total.Deployments != 0 {
		t.Errorf("deploy fuori dal periodo conteggiati: %+v", rep.Total)
	}
}

func TestRetention(t *testing.T) {
	e, w := newInsightEngine(t, nil)
	ctx := context.Background()
	for _, r := range []struct{ id, version string }{{"r1", "1.0.0"}, {"r2", "1.1.0"}, {"r3", "bad"}, {"r4", "1.2.0"}} {
		run(t, e, release(r.id, "test", r.version))
	}
	run(t, e, release("altro", "prod", "1.0.0"))

	adm := w.Admin()
	adm.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if _, err := adm.Prune(ctx, RetentionOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("max_age 0 accettato: %v", err)
	}
	o := RetentionOptions{MaxAge: 24 * time.Hour, KeepReleases: 2, DryRun: true}
	rep, err := adm.Prune(ctx, o)
	if err != nil || rep.Deleted != 2 || rep.Kept != 3 {
		t.Fatalf("dry-run inatteso: %+v %v", rep, err)
	}
	if list, _ := e.List(ctx, ListFilter{}); len(list) != 5 {
		t.Fatalf("il dry-run ha eliminato deploy: %d", len(list))
	}
	o.DryRun = false
	if _, err := adm.Prune(ctx, o); err != nil {
		t.Fatal(err)
	}
	list, _ := e.List(ctx, ListFilter{})
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	// The last 2 successful ones in test and the only one in prod remain; the oldest and the
	// failed one are gone.
	if len(ids) != 3 || !ids["r2"] || !ids["r4"] || !ids["altro"] {
		t.Fatalf("deploy rimasti inattesi: %v", ids)
	}
	inv, _ := e.Inventory(ctx, InventoryFilter{})
	if len(inv) != 2 {
		t.Errorf("inventario perso dopo la retention: %+v", inv)
	}
	// With a retention that never expires, nothing is deleted.
	adm.now = time.Now
	if rep, _ := adm.Prune(ctx, o); rep.Deleted != 0 {
		t.Errorf("eliminati deploy recenti: %+v", rep)
	}
}
