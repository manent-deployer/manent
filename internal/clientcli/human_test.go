package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/service"
)

func TestTabellaAllineataConIColori(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	p := newPalette()
	var tb table
	tb.header("ID", "STATE", "BY")
	tb.add(plain("d1"), cell{"SUCCESS", p.state}, plain("alice"))
	tb.add(plain("deploy-2"), cell{"ERROR", p.state}, plain("bob"))
	var out bytes.Buffer
	if err := tb.print(&out); err != nil {
		t.Fatal(err)
	}
	// Without the escape sequences, the third column starts at the same offset on every line.
	plainOut := strings.NewReplacer("\033[32m", "", "\033[31m", "", "\033[0m", "").Replace(out.String())
	col := -1
	for _, line := range strings.Split(strings.TrimSpace(plainOut), "\n") {
		i := strings.LastIndex(line, "  ") + 2
		if col >= 0 && i != col {
			t.Fatalf("colonne disallineate:\n%s", plainOut)
		}
		col = i
	}
	if !strings.Contains(out.String(), "\033[32mSUCCESS\033[0m") {
		t.Fatalf("stato non colorato:\n%q", out.String())
	}
	// NO_COLOR: no escape sequences.
	t.Setenv("NO_COLOR", "1")
	out.Reset()
	if err := printDeploys(&out, []service.Status{{ID: "d1", State: service.StateSuccess}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\033[") {
		t.Fatalf("colori con NO_COLOR:\n%q", out.String())
	}
}

func TestStampaDeployEInventario(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	created := now.Add(-90 * time.Second)
	deps := []service.Status{
		{ID: "shop-42", State: service.StateSuccess, Application: "shop", Artifact: "shop-web", Environment: "prod", Version: "1.4.0",
			Kind: "deploy", RequestedBy: "oidc:alice", CreatedAt: created, UpdatedAt: created.Add(12300 * time.Millisecond)},
		{ID: "shop-43", State: service.StatePending, Application: "shop", Environment: "test", Version: "1.5.0", Kind: "promotion", DryRun: true, CreatedAt: created},
	}
	var out bytes.Buffer
	if err := printDeploys(&out, deps, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"APPLICATION", "shop/shop-web", "1.4.0", "SUCCESS", "oidc:alice", "1m30s ago", "12.3s", "promotion (dry-run)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}

	inv := []service.InventoryEntry{
		{Application: "shop", Artifact: "shop", Environment: "prod", Tenant: "payments", Current: &deps[0], Drift: true,
			LastFailure: &service.Status{Version: "1.5.0", State: service.StateError}},
		// The applications without a tenant: no prefix.
		{Application: "api", Artifact: "api", Environment: "test", Tenant: "untenanted"},
	}
	out.Reset()
	if err := printInventory(&out, inv, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"payments/shop", "1.4.0", "drift", "last failure: 1.5.0 failed", "\napi "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := printStatus(&out, &deps[0], now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shop/shop-web 1.4.0 → prod", "Duration", "12.3s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}
}

// progressService answers Status and Steps with a deploy that advances at every read.
type progressService struct {
	service.Service
	reads int
}

func (s *progressService) Status(context.Context, string) (*service.Status, error) {
	s.reads++
	started := time.Unix(100, 0)
	st := &service.Status{ID: "d1", State: service.StatePending, Application: "shop", Environment: "prod", Version: "2.0.0",
		CreatedAt: started, UpdatedAt: started.Add(5 * time.Second), StartedAt: &started}
	st.Rollout = &service.RolloutState{Stages: []service.RolloutStage{{Name: "canary", State: service.StageBaking}, {Name: "eu", State: service.StagePending}}}
	if s.reads >= 2 {
		st.Rollout.Stages[0].State = service.StageSucceeded
	}
	if s.reads >= 3 {
		st.State, st.Error, st.Rollback = service.StateError, "post-test: healthz returned 500", "succeeded"
		st.Rollout.Stages[1].State, st.Rollout.Stages[1].Gate = service.StageFailed, "probe red"
	}
	return st, nil
}

func (s *progressService) Steps(context.Context, string) ([]service.StepRecord, error) {
	done := time.Unix(101, 0)
	steps := []service.StepRecord{
		{ID: 0, Name: "config", CompletedAt: &done, DurationMs: 40},
		{ID: 1, Name: "callback:deployment:0", CompletedAt: &done},
		{ID: 4, Name: "DBOS.sleep", CompletedAt: &done},
		{ID: 5, Name: "gate@canary#1", CompletedAt: &done},
		{ID: 2, Name: "deploy", CompletedAt: &done, DurationMs: 1200},
	}
	if s.reads >= 2 {
		steps = append(steps, service.StepRecord{ID: 3, Name: "post-test", Error: "healthz returned 500\nmore"})
	}
	return steps, nil
}

func TestAvanzamentoDelDeploy(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	defer func(d time.Duration) { progressInterval = d }(progressInterval)
	progressInterval = time.Millisecond
	var out bytes.Buffer
	err := watch(context.Background(), &progressService{}, "d1", "deploy", &out)
	if err == nil || err.Error() != "post-test: healthz returned 500" {
		t.Fatalf("errore atteso, ottenuto %v", err)
	}
	got := out.String()
	// Every step once, without the bookkeeping ones, and the outcome with the rollback.
	for _, want := range []string{"✓ config", "✓ deploy", "1.2s", "✗ post-test", "healthz returned 500", "✗ shop 2.0.0 → prod deploy failed after 5.0s", "✓ wave canary", "healthy", "✗ wave eu", "health gate red", "automatic rollback: succeeded"} {
		if !strings.Contains(got, want) {
			t.Errorf("manca %q in\n%s", want, got)
		}
	}
	if strings.Count(got, "config") != 1 || strings.Count(got, "wave canary") != 2 || strings.Contains(got, "callback") ||
		strings.Contains(got, "DBOS") || strings.Contains(got, "gate@") || strings.Contains(got, "more") {
		t.Errorf("passi ripetuti o nascosti mostrati:\n%s", got)
	}
}

func TestRigaDiLogSulTerminale(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	l := service.LogLine{Time: time.Date(2026, 10, 26, 10, 4, 5, 0, time.Local), Level: "WARN", Step: "rollback:deploy@canary#0", Message: "previous release restored",
		Attrs: map[string]string{"deployment": "d1", "component": "step", "host": "web-1", "release": "1.3.0", "output": "",
			"sha256": "4f47dcc221e83f4ec336e8cc62a4fe0c432e22ff287eade33e1dadf6a0e30f63"}}
	if got, want := humanLogLine(l), "10:04:05 WARN  [rollback:deploy@canary#0] previous release restored host=web-1 release=1.3.0 sha256=4f47dcc221e8…"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestLivelloDeiLog(t *testing.T) {
	if !(levelRank("ERROR") > levelRank("warn") && levelRank("WARN") > levelRank("INFO") && levelRank("INFO") > levelRank("")) {
		t.Fatal("ordine dei livelli errato")
	}
	// An unknown level in a line is not hidden without --level.
	if levelRank("NOTICE") < levelRank("") {
		t.Fatal("livello sconosciuto nascosto")
	}
}

func TestParallelNote(t *testing.T) {
	for out, want := range map[string]string{
		`{"plugin":"remote-copy"}`:                  "",
		`{"parallel":"4"}`:                          " · 4 at a time",
		`{"parallel":"1","parallel_requested":"4"}`: " · 1 at a time (4 requested)",
	} {
		var o any
		if err := json.Unmarshal([]byte(out), &o); err != nil {
			t.Fatal(err)
		}
		if got := parallelNote(o); got != want {
			t.Errorf("%s: %q, want %q", out, got, want)
		}
	}
	if parallelNote(nil) != "" {
		t.Error("no output")
	}
}
