package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/rollback"
	"github.com/joeg-ita/manent/internal/testdb"
)

// artifactEngine starts a worker and an engine with the artifact identity.
func artifactEngine(t *testing.T) *Engine {
	t.Helper()
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ByArtifact = true
	w := startWorker(t, opts)
	return NewService(ServiceOptions{
		Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, ByArtifact: true,
		Rollback: rollback.NewPolicy(config.Default(), []string{"plugin"}),
	})
}

// artifactDescriptor is a deploy of an artifact of the application app.
func artifactDescriptor(artifact, env, version string) *descriptor.Descriptor {
	d := appDescriptor(env, version)
	d.Artifact.Name = artifact
	d.Artifact.Source = "/srv/" + artifact + "-" + version + ".tar.gz"
	return d
}

func TestIdentitaPerArtefatto(t *testing.T) {
	e := artifactEngine(t)
	deploy(t, e, alice, artifactDescriptor("api", "prod", "1.0.0"))
	deploy(t, e, alice, artifactDescriptor("api", "prod", "2.0.0"))
	deploy(t, e, alice, artifactDescriptor("web", "prod", "1.0.0"))

	inv, err := e.Inventory(alice, InventoryFilter{Application: "app", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ie := range inv {
		got[ie.Artifact] = ie.Current.Version
	}
	if len(inv) != 2 || got["api"] != "2.0.0" || got["web"] != "1.0.0" {
		t.Errorf("attesa una voce per artefatto: %+v", got)
	}
	if inv, err := e.Inventory(alice, InventoryFilter{Artifact: "web"}); err != nil || len(inv) != 1 || inv[0].Current.Version != "1.0.0" {
		t.Errorf("filtro per artefatto inatteso: %+v %v", inv, err)
	}

	// Two artifacts in the environment: the rollback must say which one.
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod"}); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "api, web") {
		t.Errorf("atteso l'errore che elenca gli artefatti, ottenuto %v", err)
	}
	// The deploy of web after api 2.0.0 does not count: the rollback of api goes back to 1.0.0.
	sub, err := e.Rollback(alice, RollbackRequest{Application: "app", Artifact: "api", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(alice, sub.ID); st == nil || st.Artifact != "api" || st.Version != "1.0.0" {
		t.Errorf("rollback di api inatteso: %+v", st)
	}
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Artifact: "web", Environment: "prod"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("web ha una sola versione: atteso ErrInvalidRequest, ottenuto %v", err)
	}

	// With a single artifact in the source environment the promotion does not need it.
	deploy(t, e, alice, artifactDescriptor("api", "test", "3.0.0"))
	sub, err = e.Promote(alice, PromoteRequest{Application: "app", From: "test", To: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(alice, sub.ID); st == nil || st.Artifact != "api" || st.Version != "3.0.0" {
		t.Errorf("promozione inattesa: %+v", st)
	}

	// The artifact name becomes part of keys: it must be valid.
	bad := artifactDescriptor("team/api", "prod", "1.0.0")
	if _, err := e.Submit(alice, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso il rifiuto del nome dell'artefatto, ottenuto %v", err)
	}
}

func TestPartizionePerArtefatto(t *testing.T) {
	d := artifactDescriptor("api", "prod", "1.0.0")
	if k := partitionKey(d, false); k != "app/prod" {
		t.Errorf("chiave con l'identità per applicazione: %s", k)
	}
	if k := partitionKey(d, true); k != "app+api/prod" {
		t.Errorf("chiave con l'identità per artefatto: %s", k)
	}
	d.Tenant = "acme"
	if k := partitionKey(d, true); k != "app+api/prod/acme" {
		t.Errorf("chiave con tenant: %s", k)
	}
}

func TestAvvisoNomeArtefatto(t *testing.T) {
	e := govEngine(t, nil, nil)
	v, err := e.Validate(alice, artifactDescriptor("team/api", "prod", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Warnings) == 0 || !strings.Contains(strings.Join(v.Warnings, " "), "artifact.name") {
		t.Errorf("atteso un avviso sul nome dell'artefatto: %+v", v.Warnings)
	}
}

func TestRinominaArtefatto(t *testing.T) {
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ByArtifact = true
	w := startWorker(t, opts)
	newEngine := func() *Engine {
		return NewService(ServiceOptions{
			Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, ByArtifact: true,
			Rollback: rollback.NewPolicy(config.Default(), []string{"plugin"}),
		})
	}
	e := newEngine()
	deploy(t, e, alice, artifactDescriptor("api", "prod", "1.0.0"))
	deploy(t, e, alice, artifactDescriptor("api", "prod", "2.0.0"))

	adm := NewAdmin(AdminOptions{Client: w.Client(), Logger: discard})
	rep, err := adm.RenameArtifact(alice, RenameArtifactOptions{Application: "app", From: "api", To: "backend", DryRun: true})
	if err != nil || rep.Renamed != 2 {
		t.Fatalf("dry-run: %+v %v", rep, err)
	}
	if rep, err = adm.RenameArtifact(alice, RenameArtifactOptions{Application: "app", From: "api", To: "backend"}); err != nil || rep.Renamed != 2 {
		t.Fatalf("rinomina: %+v %v", rep, err)
	}
	if _, err := adm.RenameArtifact(alice, RenameArtifactOptions{Application: "app", From: "backend", To: "team/api"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("nome non valido accettato: %v", err)
	}

	// A new engine rebuilds the inventory: the history follows the new name.
	e = newEngine()
	inv, err := e.Inventory(alice, InventoryFilter{Application: "app"})
	if err != nil || len(inv) != 1 || inv[0].Artifact != "backend" || inv[0].Current.Version != "2.0.0" {
		t.Fatalf("inventario dopo la rinomina: %+v %v", inv, err)
	}
	sub, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(alice, sub.ID); st == nil || st.Artifact != "backend" || st.Version != "1.0.0" {
		t.Errorf("il rollback deve usare il nuovo nome: %+v", st)
	}
}
