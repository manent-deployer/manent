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
	ws[0].Queues = []string{"deployments", "deployments-approvals"}
	ws[0].Resources = &service.WorkerResources{CPUs: 8, CPULimit: 2, ProcessCPU: 0.35, ProcessMemory: 64 << 20,
		MemoryTotal: 16 << 30, MemoryLimit: 1 << 30, Load1: 0.5, WorkdirFree: 10 << 30, Concurrency: 4}
	var out bytes.Buffer
	if err := printWorkers(&out, ws, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"w1", "alive", "eu", "node-1", "1 (v0.9.0)", "3s fa", "1 (+1 non visibili)", "w2", "stale", "deploy", "api/prod",
		"0.35/2 (host 8)", "64.0 MiB/1.0 GiB", "10.0 GiB liberi", "deployments, deployments-approvals"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}

	at := now.Add(time.Hour)
	a := &service.Activity{
		Running: []service.Status{{ID: "d1", Application: "api", Environment: "prod", Version: "1.2.0", Executor: "w1", LastStep: "deploy", StartedAt: &beat, RolloutProgress: "1/3"}},
		Queued:  []service.Status{{ID: "d2", Application: "api", Environment: "prod", Queue: "deployments", CreatedAt: beat}},
		WaitingDependency: []service.Status{{ID: "d4", Application: "web", Environment: "prod", CreatedAt: beat,
			After: &service.AfterState{Dependencies: []service.DependencyState{{ID: "d1", State: "PENDING"}, {ID: "d0", State: "SUCCESS"}}}}},
		Delayed: []service.Status{{ID: "d3", Application: "api", Environment: "test", ScheduledAt: &at, DryRun: true}},
	}
	out.Reset()
	if err := printActivity(&out, a, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"In attesa delle dipendenze (1)", "attende d1 (PENDING)", "In esecuzione (1)", "step deploy, ondate 1/3", "In coda (1)", "deployments", "Pianificati (1)", "d3 (dry-run)", "tra 1h0m0s"} {
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

func TestStampaCode(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	oldest := now.Add(-90 * time.Second)
	qs := []service.QueueInfo{
		{Name: "deployments", Kind: service.QueueDeployments, WorkerConcurrency: 4, PartitionConcurrency: 1, Enqueued: 3, Running: 2, Delayed: 1,
			OldestEnqueued: &oldest, Workers: []string{"w1", "w2"}},
		{Name: "deployments.eu", Kind: service.QueueRegion, Region: "eu", Workers: []string{}},
	}
	var out bytes.Buffer
	if err := printQueues(&out, qs, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deployments", "1m30s", "4 per worker, 1 per ambiente", "2 (w1, w2)", "region eu", "nessuno"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manca %q in\n%s", want, out.String())
		}
	}
}
