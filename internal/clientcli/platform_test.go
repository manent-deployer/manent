package clientcli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/service"
)

func TestStampaWorkerEAttivita(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	beat, started := now.Add(-3*time.Second), now.Add(-2*time.Hour)
	ws := []service.WorkerInfo{
		{ID: "w1", State: service.WorkerAlive, Region: "eu", Hostname: "node-1", Version: "1", Build: "v0.9.0",
			HeartbeatAt: &beat, StartedAt: &started, HiddenTasks: 1,
			Tasks: []service.WorkerTask{{ID: "d1", DeploymentID: "d1", Kind: service.TaskDeploy, Application: "api", Environment: "prod", StartedAt: &beat}}},
		{ID: "w2", State: service.WorkerStale, Tasks: []service.WorkerTask{}},
	}
	var out bytes.Buffer
	if err := printWorkers(&out, ws, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"w1", "alive", "eu", "node-1", "1 (v0.9.0)", "3s fa", "1 (+1 non visibili)", "w2", "stale", "deploy", "api/prod"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}

	at := now.Add(time.Hour)
	a := &service.Activity{
		Running: []service.Status{{ID: "d1", Application: "api", Environment: "prod", Version: "1.2.0", Executor: "w1", LastStep: "deploy", StartedAt: &beat, RolloutProgress: "1/3"}},
		Queued:  []service.Status{{ID: "d2", Application: "api", Environment: "prod", Queue: "deployments", CreatedAt: beat}},
		Delayed: []service.Status{{ID: "d3", Application: "api", Environment: "test", ScheduledAt: &at, DryRun: true}},
	}
	out.Reset()
	if err := printActivity(&out, a, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"In esecuzione (1)", "step deploy, ondate 1/3", "In coda (1)", "deployments", "Pianificati (1)", "d3 (dry-run)", "tra 1h0m0s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "approvazione") {
		t.Errorf("sezione vuota stampata:\n%s", out.String())
	}
	out.Reset()
	_ = printActivity(&out, &service.Activity{}, now)
	if !strings.Contains(out.String(), "Nessun deploy attivo") {
		t.Errorf("attività vuota: %s", out.String())
	}
}
