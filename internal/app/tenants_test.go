package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/internal/config"
)

// The queue conflicts between the tenant registry and the configuration explain how to fix
// them.
func TestCodeDeiTenantErrori(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tenants.yml")
	registry := "tenants:\n  avengers:\n    applications: {hulk: {}}\n    queue: deployments.heros\n  evils:\n    applications: {loki: {}}\n"
	if err := os.WriteFile(file, []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	newCfg := func() *config.Config {
		cfg := config.Default()
		cfg.Tenants.File = file
		return cfg
	}

	// The same queue as a tenant's and as a queues entry: both ways out are named.
	cfg := newCfg()
	cfg.Queues = []config.QueueRoute{{Name: "deployments.heros", Match: config.EnvMatch{Environments: []string{"prod"}}}}
	_, _, err := tenantQueues(cfg, NewTenantCache(cfg, log), log)
	if err == nil {
		t.Fatal("a queue both of a tenant and of queues must be refused")
	}
	for _, want := range []string{"queue deployments.heros is also in queues", "remove queue (and concurrency) from tenant avengers in " + file, "remove the queues entry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}

	// An unknown tenant in dbos.tenants: the registry's tenants are listed.
	cfg = newCfg()
	cfg.DBOS.Tenants = []string{"avenger"}
	_, _, err = tenantQueues(cfg, NewTenantCache(cfg, log), log)
	if err == nil || !strings.Contains(err.Error(), "tenant avenger is not in the registry") || !strings.Contains(err.Error(), "avengers, evils") {
		t.Errorf("unknown tenant: %v", err)
	}

	// dbos.tenants without a registry.
	cfg = config.Default()
	cfg.DBOS.Tenants = []string{"avengers"}
	if _, _, err = tenantQueues(cfg, nil, log); err == nil || !strings.Contains(err.Error(), "set tenants.file") {
		t.Errorf("no registry: %v", err)
	}
}
