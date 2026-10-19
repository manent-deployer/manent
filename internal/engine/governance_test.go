package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollback"
	"github.com/joeg-ita/manent/internal/testdb"
)

// artifactStep simulates the artifacts step: it returns the artifact's sha256.
type artifactStep struct{}

func (artifactStep) Name() string { return "artifacts" }

func (artifactStep) Run(_ context.Context, in pipeline.Input) (pipeline.Output, error) {
	if strings.HasSuffix(in.Descriptor.Artifact.Version, "-broken") {
		return nil, pipeline.Permanent(errors.New("release difettosa"))
	}
	return pipeline.Output{"sha256": "sha-" + in.Descriptor.Artifact.Version}, nil
}

var (
	alice = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Method: auth.MethodOIDC})
	bob   = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "bob", Method: auth.MethodOIDC})
	carol = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "carol", Method: auth.MethodOIDC})
)

// govEngine starts a worker and an engine with the given rules.
func govEngine(t *testing.T, rules []governance.Rule, windows []governance.Window) *Engine {
	t.Helper()
	gov, err := governance.New(rules, windows)
	if err != nil {
		t.Fatal(err)
	}
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Governance = gov
	w := startWorker(t, opts)
	cfg := config.Default()
	return NewService(ServiceOptions{
		Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		Governance: gov, Rollback: rollback.NewPolicy(cfg, []string{"plugin"}),
	})
}

func appDescriptor(env, version string) *descriptor.Descriptor {
	d := testDescriptor("")
	d.Environment = env
	d.Artifact = descriptor.Artifact{Type: "application", Name: "app", Version: version, Source: "/srv/app-" + version + ".tar.gz"}
	return d
}

// deploy enqueues the deploy and waits for it to end.
func deploy(t *testing.T, e *Engine, ctx context.Context, d *descriptor.Descriptor) string {
	t.Helper()
	sub, err := e.Submit(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	return sub.ID
}

func TestInventarioEPromozione(t *testing.T) {
	e := govEngine(t, []governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"}, Requires: []string{"test"}}}, nil)

	// 2.0.0 was not released in test: it is not allowed in prod.
	if _, err := e.Submit(alice, appDescriptor("prod", "2.0.0")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("atteso ErrInvalidRequest per una versione non promossa, ottenuto %v", err)
	}
	deploy(t, e, alice, appDescriptor("test", "1.0.0"))
	testID := deploy(t, e, alice, appDescriptor("test", "2.0.0"))
	// Dry-runs are not subject to the rules.
	dry := appDescriptor("prod", "3.0.0")
	dry.DryRun = true
	deploy(t, e, alice, dry)

	sub, err := e.Promote(alice, PromoteRequest{Application: "app", From: "test", To: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(alice, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind != KindPromotion || st.Source != testID || st.Version != "2.0.0" || st.SHA256 != "sha-2.0.0" || st.Environment != "prod" {
		t.Errorf("stato della promozione inatteso: %+v", st)
	}
	// A version never released in the source environment cannot be promoted.
	if _, err := e.Promote(alice, PromoteRequest{Application: "app", From: "test", To: "prod", Version: "9.9.9"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso ErrInvalidRequest, ottenuto %v", err)
	}

	inv, err := e.Inventory(alice, InventoryFilter{Application: "app"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ie := range inv {
		got[ie.Environment] = ie.Current.Version
	}
	if len(inv) != 2 || got["test"] != "2.0.0" || got["prod"] != "2.0.0" {
		t.Errorf("inventario inatteso (il dry-run non conta): %+v", got)
	}

	list, err := e.List(alice, ListFilter{Environment: "test", State: "SUCCESS", Limit: 1})
	if err != nil || len(list) != 1 || list[0].Version != "2.0.0" {
		t.Errorf("elenco filtrato inatteso: %+v %v", list, err)
	}
	// The Match filter does not reduce the number of results below the limit.
	list, err = e.List(alice, ListFilter{Limit: 2, Match: func(s Status) bool { return s.Version == "1.0.0" || s.Kind == KindPromotion }})
	if err != nil || len(list) != 2 {
		t.Errorf("elenco con Match: attesi 2 deploy, ottenuti %+v %v", list, err)
	}
}

func TestApprovazioni(t *testing.T) {
	e := govEngine(t, []governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"},
		Approvals: governance.Approvals{Required: 2, Timeout: time.Minute}}}, nil)

	d := appDescriptor("prod", "1.0.0")
	d.ID = "rel-1"
	sub, err := e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	if sub.ID != "rel-1" || sub.State != StateAwaitingApproval {
		t.Fatalf("submission inattesa: %+v", sub)
	}
	waitState(t, e, "rel-1", StateAwaitingApproval, func(s *Status) bool { return s.Approval != nil })

	// Self-approval is discarded by the workflow even if it gets there.
	if err := e.Decide(alice, "rel-1", DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(bob, "rel-1", DecisionRequest{Approve: true, Comment: "ok"}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e, "rel-1", StateAwaitingApproval, func(s *Status) bool { return len(s.Approval.Decisions) == 1 })
	if st.Approval.Decisions[0].Approver != "oidc:bob" || st.RequestedBy != "oidc:alice" {
		t.Errorf("decisioni inattese: %+v", st.Approval)
	}
	if err := e.Decide(bob, "rel-1", DecisionRequest{Approve: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("una seconda decisione dello stesso approvatore va rifiutata, ottenuto %v", err)
	}
	if err := e.Decide(carol, "rel-1", DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(alice, 30*time.Second)
	defer cancel()
	if _, err := e.Wait(ctx, "rel-1"); err != nil {
		t.Fatal(err)
	}
	st, err = e.Status(alice, "rel-1")
	if err != nil || st.State != "SUCCESS" || st.Approval == nil || st.Approval.Status != ApprovalApproved {
		t.Errorf("stato dopo l'approvazione inatteso: %+v %v", st, err)
	}

	// A rejection closes the request.
	d2 := appDescriptor("prod", "1.1.0")
	sub, err = e.Submit(alice, d2)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(bob, sub.ID, DecisionRequest{Approve: false, Comment: "manca il change"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err == nil {
		t.Error("un deploy respinto non deve risultare eseguito")
	}
	waitState(t, e, sub.ID, StateRejected, nil)
	list, err := e.List(alice, ListFilter{Environment: "prod"})
	if err != nil || len(list) != 2 || list[0].State != StateRejected || list[1].ID != "rel-1" {
		t.Errorf("elenco inatteso: %+v %v", list, err)
	}

	// A cancelled request does not run.
	sub, err = e.Submit(alice, appDescriptor("prod", "1.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, sub.ID, "CANCELLED", nil)
}

func TestApprovazioneScaduta(t *testing.T) {
	e := govEngine(t, []governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"},
		Approvals: governance.Approvals{Required: 1, Timeout: 500 * time.Millisecond}}}, nil)
	sub, err := e.Submit(alice, appDescriptor("prod", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e, sub.ID, StateExpired, nil)
}

func TestFreeze(t *testing.T) {
	now := time.Now()
	e := govEngine(t, nil, []governance.Window{{
		Name: "rilascio-trimestrale", Reason: "chiusura contabile", Applications: []string{"*"}, Environments: []string{"prod"},
		From: now.Add(-time.Hour), To: now.Add(time.Hour),
	}})
	_, err := e.Submit(alice, appDescriptor("prod", "1.0.0"))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("atteso il blocco del freeze, ottenuto %v", err)
	}
	deploy(t, e, alice, appDescriptor("test", "1.0.0"))

	d := appDescriptor("prod", "1.0.0")
	d.FreezeOverride = "INC-42 correzione urgente"
	id := deploy(t, e, alice, d)
	st, err := e.Status(alice, id)
	if err != nil || st.Result.Steps[0].Output["freeze_override"] != d.FreezeOverride {
		t.Errorf("la deroga va registrata nello step config: %+v %v", st, err)
	}
	ws, err := e.Freezes(alice, "app", "prod")
	if err != nil || len(ws) != 1 || !ws[0].Active {
		t.Errorf("finestre inattese: %+v %v", ws, err)
	}
}

func TestFreezeVerificatoDalWorker(t *testing.T) {
	now := time.Now()
	gov, err := governance.New(nil, []governance.Window{{Name: "f", Applications: []string{"*"}, Environments: []string{"prod"}, From: now.Add(-time.Hour), To: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Governance = gov
	w := startWorker(t, opts)
	// An engine without governance (e.g. deploy enqueued before the freeze) does not block the
	// request: the worker blocks it at startup, without retries.
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	sub, err := e.Submit(alice, appDescriptor("prod", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err == nil || !strings.Contains(err.Error(), "freeze") {
		t.Errorf("atteso il blocco del freeze nel worker, ottenuto %v", err)
	}
}

func TestRollbackManuale(t *testing.T) {
	e := govEngine(t, nil, nil)
	v1 := deploy(t, e, alice, appDescriptor("prod", "1.0.0"))
	deploy(t, e, alice, appDescriptor("prod", "2.0.0"))
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod", Version: "2.0.0"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("la versione in esercizio non è un rollback: %v", err)
	}
	sub, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	inv, err := e.Inventory(alice, InventoryFilter{Environment: "prod"})
	if err != nil || len(inv) != 1 || inv[0].Current.Version != "1.0.0" || inv[0].Current.Kind != KindRollback || inv[0].Current.Source != v1 {
		t.Errorf("inventario dopo il rollback inatteso: %+v %v", inv, err)
	}

	// A failed deploy without automatic rollback leaves the targets changed: the version of the
	// last successful deploy can be restored, and it is the rollback default.
	sub, err = e.Submit(alice, appDescriptor("prod", "3.0.0-broken"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err == nil {
		t.Fatal("il deploy difettoso doveva fallire")
	}
	sub, err = e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod"})
	if err != nil {
		t.Fatalf("ripristino dopo un deploy fallito: %v", err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(alice, sub.ID); st == nil || st.Version != "1.0.0" {
		t.Errorf("il rollback doveva ripristinare la 1.0.0: %+v", st)
	}
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod", Version: "1.0.0"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("dopo il ripristino la 1.0.0 è di nuovo in esercizio: %v", err)
	}
	sub, err = e.Submit(alice, appDescriptor("prod", "4.0.0-broken"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err == nil {
		t.Fatal("il deploy difettoso doveva fallire")
	}
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod", Version: "1.0.0"}); err != nil {
		t.Errorf("--to con la versione dell'ultimo deploy riuscito dopo un fallimento: %v", err)
	}

	// Migrations do not allow rollback.
	m1 := appDescriptor("db", "1")
	m1.Artifact.Type = "migration"
	deploy(t, e, alice, m1)
	m2 := appDescriptor("db", "2")
	m2.Artifact.Type = "migration"
	deploy(t, e, alice, m2)
	if _, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "db"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso rollback non ammesso per le migration, ottenuto %v", err)
	}
}

func TestMessaggiSenzaReleaseDaUsare(t *testing.T) {
	e := govEngine(t, nil, nil)
	deploy(t, e, alice, appDescriptor("prod", "1.0.0"))

	// First release: there is no previous version to go back to.
	_, err := e.Rollback(alice, RollbackRequest{Application: "app", Environment: "prod"})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "no version before 1.0.0 was released successfully in prod") {
		t.Errorf("errore del rollback senza versione precedente inatteso: %v", err)
	}
	// Nothing was ever released in the source environment.
	_, err = e.Promote(alice, PromoteRequest{Application: "app", From: "staging", To: "prod"})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "app: no version was released successfully in staging") {
		t.Errorf("errore della promozione senza release di origine inatteso: %v", err)
	}
}

// waitState waits until the deploy reaches the given state (and condition, if any).
func waitState(t *testing.T, e *Engine, id, state string, cond func(*Status) bool) *Status {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := e.Status(context.Background(), id)
		if err == nil && st.State == state && (cond == nil || cond(st)) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("deploy %s: stato %+v (errore %v), atteso %s", id, st, err, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// policyResolver simulates the environment configuration: it rejects "vietata" and is
// unreachable for "irraggiungibile".
type policyResolver struct{}

func (policyResolver) Resolve(_ context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	switch d.Application {
	case "vietata":
		return d, nil, pipeline.Permanent(errors.New("descrittore non ammesso dalla policy"))
	case "irraggiungibile":
		return d, nil, errors.New("consul non raggiungibile")
	}
	return d, map[string]string{}, nil
}

func TestPolicyVerificataAllAccodamento(t *testing.T) {
	reg, planner := testRegistry(t, artifactStep{})
	w := startWorker(t, workerOpts(testdb.URL(t), "w1", reg))
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Resolver: policyResolver{}})
	d := appDescriptor("test", "1.0.0")
	d.Application = "vietata"
	if _, err := e.Submit(alice, d); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("violazione della policy: atteso ErrInvalidRequest, ricevuto %v", err)
	}
	// If the configuration is unreachable the check is left to the worker.
	d.Application = "irraggiungibile"
	if _, err := e.Submit(alice, d); err != nil {
		t.Errorf("configurazione non raggiungibile: la richiesta va accettata, ricevuto %v", err)
	}
}

func TestScheduleConApprovazione(t *testing.T) {
	e := govEngine(t, []governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"},
		Approvals: governance.Approvals{Required: 1, Timeout: time.Minute}}}, nil)
	d := appDescriptor("prod", "1.0.0")
	d.Schedule = &descriptor.Schedule{Cron: "* * * * * *", Name: "ogni-secondo"}
	if _, err := e.Submit(alice, d); err != nil {
		t.Fatal(err)
	}
	var id string
	deadline := time.Now().Add(20 * time.Second)
	for id == "" {
		list, err := e.List(alice, ListFilter{Schedule: "ogni-secondo", State: StateAwaitingApproval, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			id = list[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatal("nessuna esecuzione della schedule in attesa di approvazione")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := e.DeleteSchedule(alice, "ogni-secondo"); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(bob, id, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, id, "SUCCESS", nil)
}

func TestApprovazioniInAttesaECursore(t *testing.T) {
	e := govEngine(t, []governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"},
		Approvals: governance.Approvals{Required: 1, Timeout: time.Minute}}}, nil)
	deploy(t, e, alice, appDescriptor("test", "1.0.0"))
	deploy(t, e, alice, appDescriptor("test", "1.1.0"))
	sub, err := e.Submit(alice, appDescriptor("prod", "1.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	// The approval state is published as soon as its workflow starts.
	var list []Status
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if list, err = e.Approvals(alice); err == nil && len(list) == 1 && list[0].Approval != nil {
			break
		}
	}
	if err != nil || len(list) != 1 || list[0].ID != sub.ID || list[0].Approval == nil || list[0].Approval.Required != 1 {
		t.Fatalf("approvazioni in attesa: %+v %v", list, err)
	}
	if err := e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.Approvals(alice); len(list) != 0 {
		t.Errorf("approvato: non è più in attesa: %+v", list)
	}

	// Pagination: the second page restarts after the last deploy of the first.
	page1, _ := e.List(alice, ListFilter{Limit: 2})
	page2, _ := e.List(alice, ListFilter{Limit: 2, Cursor: page1[1].ID})
	if len(page1) != 2 || len(page2) != 1 || page2[0].ID == page1[0].ID || page2[0].ID == page1[1].ID {
		t.Errorf("pagine: %v / %v", ids(page1), ids(page2))
	}
}

func ids(ss []Status) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
