package clientcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/service"
)

func TestDeployDallaCI(t *testing.T) {
	var got descriptor.Descriptor
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/deployments":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(service.Submission{ID: got.ID, State: service.StateEnqueued})
		case r.URL.Path == "/api/v1/deployments/"+got.ID:
			_ = json.NewEncoder(w).Encode(service.Status{ID: got.ID, State: service.StatePending})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	desc := filepath.Join(dir, "deploy.yml")
	body := "apiVersion: manent/v1\napplication: shop\nenvironment: prod\nartifact: {type: application, name: shop, version: \"0.0.0\", source: \"file:///srv/shop.tgz\"}\n"
	if err := os.WriteFile(desc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "github_output")
	if err := os.WriteFile(out, []byte("prima=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MANENT_TOKEN", "t")
	err := Run([]string{"--server", srv.URL, "deploy", "-f", desc, "--no-wait", "--id", "ci-42", "--version", "1.4.0",
		"--source", "file:///srv/shop-1.4.0.tgz", "--outputs", out})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ci-42" || got.Artifact.Version != "1.4.0" || got.Artifact.Source != "file:///srv/shop-1.4.0.tgz" {
		t.Errorf("descrittore inviato: %+v", got)
	}
	raw, _ := os.ReadFile(out)
	// Appended, like GITHUB_OUTPUT: the previous lines stay.
	for _, want := range []string{"prima=1\n", "deployment_id=ci-42\n", "state=PENDING\n", "version=1.4.0\n", "environment=prod\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("esiti senza %q:\n%s", want, raw)
		}
	}
}

func TestDeployCIRiconosceLaPiattaforma(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(service.Submission{ID: "d9", State: service.StateEnqueued})
			return
		}
		_ = json.NewEncoder(w).Encode(service.Status{ID: "d9", State: service.StateError, Error: "post-test | fallito", Rollback: "completed"})
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	desc := filepath.Join(dir, "deploy.yml")
	_ = os.WriteFile(desc, []byte("apiVersion: manent/v1\napplication: shop\nenvironment: prod\nartifact: {type: application, name: shop, version: \"2.0.0\", source: \"file:///srv/shop.tgz\"}\n"), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MANENT_TOKEN", "t")
	run := func() error { return Run([]string{"--server", srv.URL, "deploy", "-f", desc, "--no-wait", "--ci"}) }

	// GitHub Actions: outputs and summary of the job.
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(dir, "out"))
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary.md"))
	if err := run(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "out"))
	sum, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	if !strings.Contains(string(out), "state=ERROR\n") || !strings.Contains(string(sum), "| Esito | **ERROR** |") ||
		!strings.Contains(string(sum), `post-test \| fallito`) || !strings.Contains(string(sum), "| Rollback | completed |") {
		t.Errorf("GitHub:\n%s\n%s", out, sum)
	}

	// GitLab CI: dotenv report in the working directory.
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GITLAB_CI", "true")
	t.Chdir(dir)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "manent.env")); !strings.Contains(string(env), "deployment_id=d9\n") {
		t.Errorf("GitLab: %s", env)
	}

	// Neither: --ci is an error before submitting anything.
	t.Setenv("GITLAB_CI", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "--ci") {
		t.Errorf("senza CI: %v", err)
	}
}

func TestDeployConImportEdExport(t *testing.T) {
	var got descriptor.Descriptor
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/deployments":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(service.Submission{ID: got.ID, State: service.StateEnqueued})
		case r.URL.Path == "/api/v1/deployments/"+got.ID:
			_ = json.NewEncoder(w).Encode(service.Status{ID: got.ID, State: service.StateSuccess,
				Exports: map[string]string{"fqdn": "web1.internal", "notes": "riga1\nriga2"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	desc := filepath.Join(dir, "deploy.yml")
	// targetsFrom uses an import that only --import declares: the descriptor is validated after the options.
	body := "apiVersion: manent/v1\napplication: os\nenvironment: prod\nartifact: {type: ansible, name: os, version: \"1.0.0\", source: \"file:///srv/os.tgz\"}\n" +
		"deploy:\n  targetsFrom: [{import: infra, export: vms}]\n"
	if err := os.WriteFile(desc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "outputs")
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MANENT_TOKEN", "t")
	if err := Run([]string{"--server", srv.URL, "deploy", "-f", desc, "--no-wait", "--id", "os-7", "--import", "infra=infra-42", "--outputs", out}); err != nil {
		t.Fatal(err)
	}
	if got.Imports["infra"].Deployment != "infra-42" || len(got.After) != 1 || got.After[0] != "infra-42" {
		t.Errorf("descrittore inviato: %+v %+v", got.Imports, got.After)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), "export_fqdn=web1.internal\n") || strings.Contains(string(raw), "export_notes") {
		t.Errorf("export negli esiti:\n%s", raw)
	}
	if err := Run([]string{"--server", srv.URL, "deploy", "-f", desc, "--no-wait", "--import", "senza-id"}); err == nil || !strings.Contains(err.Error(), "expected NAME=ID") {
		t.Errorf("--import non valido: %v", err)
	}
}
