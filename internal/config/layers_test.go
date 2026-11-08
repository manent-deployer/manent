package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeFiles writes the YAML documents to files of a temporary directory, in order.
func writeFiles(t *testing.T, docs ...string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(docs))
	for i, doc := range docs {
		paths[i] = filepath.Join(dir, []string{"base.yml", "role.yml", "extra.yml"}[i])
		if err := os.WriteFile(paths[i], []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// Several files are layers on the same Config: a later file overrides only what it sets.
func TestFileAStrati(t *testing.T) {
	base := `
app: {name: shared}
dbos: {executor_id: w1, database_url: "sqlite:shared.db"}
callbacks: {timeout: 15s, allowed_hosts: [a.example.com, b.example.com]}
plugins: {jboss: {url: "http://jboss:9990"}}
rollback: {artifact_types: {application: {allowed: true, enabled: true}}}
auth: {mode: enabled, oidc: {issuer: "https://idp.example.com", username_claim: preferred_username}}
queues: [{name: gpu, match: {environments: [prod]}}]
artifacts: {sources: [{prefix: "https://nexus.example.com/"}]}
`
	role := `
callbacks: {allowed_hosts: [c.example.com]}
plugins: {script: {}}
rollback: {artifact_types: {application: {allowed: true}}}
auth: {oidc: {audience: manent}}
dbos: {executor_id: w2, queues: [gpu]}
artifacts: {sources: []}
`
	cfg, err := Load(writeFiles(t, base, role)...)
	if err != nil {
		t.Fatal(err)
	}
	// Scalars: replaced where set, kept elsewhere.
	if cfg.App.Name != "shared" || cfg.DBOS.ExecutorID != "w2" || cfg.DBOS.DatabaseURL != "sqlite:shared.db" || cfg.Callbacks.Timeout != 15*time.Second {
		t.Errorf("scalars: app %q, executor %q, db %q, timeout %v", cfg.App.Name, cfg.DBOS.ExecutorID, cfg.DBOS.DatabaseURL, cfg.Callbacks.Timeout)
	}
	// Lists: replaced whole; [] clears.
	if !slices.Equal(cfg.Callbacks.AllowedHosts, []string{"c.example.com"}) || len(cfg.Artifacts.Sources) != 0 {
		t.Errorf("lists: hosts %v, sources %v", cfg.Callbacks.AllowedHosts, cfg.Artifacts.Sources)
	}
	// Sections: merged field by field, through pointers too.
	if o := cfg.Auth.OIDC; o == nil || o.Issuer != "https://idp.example.com" || o.Audience != "manent" || o.UsernameClaim != "preferred_username" {
		t.Errorf("oidc: %+v", cfg.Auth.OIDC)
	}
	// Maps: keys added (remote-copy is the default one).
	for _, name := range []string{"jboss", "script", "remote-copy"} {
		if _, ok := cfg.Plugins[name]; !ok {
			t.Errorf("plugin %s missing: %v", name, cfg.Plugins)
		}
	}
	// A map entry is replaced whole: enabled is not inherited from the base.
	if rt := cfg.Rollback.ArtifactTypes["application"]; !rt.Allowed || rt.Enabled {
		t.Errorf("rollback entry: %+v", rt)
	}
	// Validation applies to the result: dbos.queues of the role names a queue of the base.
	if !slices.Equal(cfg.DBOS.Queues, []string{"gpu"}) || cfg.QueueRoute("gpu") == nil {
		t.Errorf("queues: %v %v", cfg.DBOS.Queues, cfg.Queues)
	}
}

// The role file alone would not be valid: validation runs once, on the layered result.
func TestFileAStratiValidazioneSulRisultato(t *testing.T) {
	paths := writeFiles(t, "queues: [{name: gpu, match: {environments: [prod]}}]\n", "dbos: {executor_id: w1, queues: [gpu]}\n")
	if _, err := Load(paths[1]); err == nil || !strings.Contains(err.Error(), "queue gpu is not in queues") {
		t.Errorf("the role file alone: %v", err)
	}
	if _, err := Load(paths...); err != nil {
		t.Errorf("base + role: %v", err)
	}
}

// With several files an error names the file; an empty file (only comments) is a valid layer.
func TestFileAStratiErrori(t *testing.T) {
	paths := writeFiles(t, "dbos: {executor_id: w1}\n", "# nothing to add for this role\n", "callbacks: {timeuot: 5s}\n")
	if _, err := Load(paths[:2]...); err != nil {
		t.Errorf("an empty layer must be accepted: %v", err)
	}
	_, err := Load(paths...)
	if err == nil || !strings.HasPrefix(err.Error(), paths[2]+": invalid configuration") || !strings.Contains(err.Error(), "timeuot") {
		t.Errorf("unknown key in the third file: %v", err)
	}
	removed := writeFiles(t, "dbos: {executor_id: w1}\n", "envconfig: {provider: consul}\n")
	if _, err := Load(removed...); err == nil || !strings.HasPrefix(err.Error(), removed[1]+": ") || !strings.Contains(err.Error(), "envconfig.providers") {
		t.Errorf("removed key in the second file: %v", err)
	}
	if _, err := Load(paths[0], filepath.Join(t.TempDir(), "missing.yml")); err == nil || !strings.Contains(err.Error(), "missing.yml") {
		t.Errorf("missing file: %v", err)
	}
}
