package authz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
)

func testPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := NewPolicy(map[string][]Action{"canceller": {DeploymentRead, DeploymentCancel}}, []Binding{
		{Groups: []string{"team-a"}, Role: "deployer", Applications: []string{"team-a-*"}, Environments: []string{"dev", "test"}},
		{Groups: []string{"team-a"}, Role: "tester", Applications: []string{"team-a-*"}, Environments: []string{"prod"}},
		{Groups: []string{"release-managers"}, Role: "operator", Applications: []string{"*"}, Environments: []string{"prod"}},
		{Subjects: []string{"token:ci"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"test"}},
		{Subjects: []string{"cli:*"}, Role: "viewer", Applications: []string{"*"}, Environments: []string{"*"}},
		{Subjects: []string{"oidc:ops"}, Role: "operator", Applications: []string{"team-a-*"}, Environments: []string{"test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicy(t *testing.T) {
	pol := testPolicy(t)
	alice := &auth.Principal{Subject: "alice", Groups: []string{"team-a"}, Method: auth.MethodOIDC}
	bob := &auth.Principal{Subject: "bob", Groups: []string{"release-managers"}, Method: auth.MethodOIDC}
	ci := &auth.Principal{Subject: "ci", Method: auth.MethodToken}
	op := &auth.Principal{Subject: "joeg", Method: auth.MethodCLI}
	tests := []struct {
		p        *auth.Principal
		a        Action
		app, env string
		want     bool
	}{
		{alice, DeploymentSubmit, "team-a-api", "test", true},
		{alice, DeploymentSubmit, "team-a-api", "prod", false},
		{alice, DeploymentDryRun, "team-a-api", "prod", true},
		{alice, DeploymentRead, "team-b-api", "test", false},
		{bob, ScheduleWrite, "team-b-api", "prod", true},
		{bob, DeploymentSubmit, "team-b-api", "test", false},
		{ci, DeploymentSubmit, "qualsiasi", "test", true},
		{ci, DeploymentCancel, "qualsiasi", "prod", false},
		{op, DeploymentRead, "x", "prod", true},
		{op, DeploymentCancel, "x", "prod", false},
		{nil, DeploymentRead, "x", "prod", false},
	}
	for _, tt := range tests {
		if got := pol.Allowed(tt.p, tt.a, tt.app, tt.env); got != tt.want {
			t.Errorf("%s %s %s/%s = %v, atteso %v", tt.p.String(), tt.a, tt.app, tt.env, got, tt.want)
		}
	}
	if !(Policy{Disabled: true}).Allowed(nil, ScheduleWrite, "x", "y") {
		t.Error("una policy disattivata concede tutto")
	}
}

func TestNewPolicyErrori(t *testing.T) {
	cases := map[string]struct {
		roles    map[string][]Action
		bindings []Binding
	}{
		"ruolo predefinito ridefinito": {roles: map[string][]Action{"admin": {DeploymentRead}}},
		"azione sconosciuta":           {roles: map[string][]Action{"x": {"deployment.destroy"}}},
		"ruolo sconosciuto":            {bindings: []Binding{{Groups: []string{"g"}, Role: "boss", Applications: []string{"*"}, Environments: []string{"*"}}}},
		"senza soggetti":               {bindings: []Binding{{Role: "viewer", Applications: []string{"*"}, Environments: []string{"*"}}}},
		"senza ambienti":               {bindings: []Binding{{Groups: []string{"g"}, Role: "viewer", Applications: []string{"*"}}}},
	}
	for name, c := range cases {
		if _, err := NewPolicy(c.roles, c.bindings); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
}

// fakeService è un engine.Service in memoria.
type fakeService struct {
	statuses  map[string]engine.Status
	schedules map[string]engine.ScheduleInfo
	submitted []*descriptor.Descriptor
	cancelled []string
	decided   []string
}

func (f *fakeService) Plan(context.Context, *descriptor.Descriptor) (pipeline.Plan, error) {
	return nil, nil
}
func (f *fakeService) Submit(_ context.Context, d *descriptor.Descriptor) (*engine.Submission, error) {
	f.submitted = append(f.submitted, d)
	return &engine.Submission{ID: "new"}, nil
}
func (f *fakeService) Status(_ context.Context, id string) (*engine.Status, error) {
	st, ok := f.statuses[id]
	if !ok {
		return nil, engine.ErrNotFound
	}
	return &st, nil
}
func (f *fakeService) List(_ context.Context, lf engine.ListFilter) ([]engine.Status, error) {
	var out []engine.Status
	for _, id := range []string{"a1", "b1"} {
		if lf.Match == nil || lf.Match(f.statuses[id]) {
			out = append(out, f.statuses[id])
		}
	}
	return out, nil
}
func (f *fakeService) Wait(context.Context, string) (*engine.Result, error) {
	return &engine.Result{}, nil
}
func (f *fakeService) Cancel(_ context.Context, id string) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeService) ListSchedules(context.Context) ([]engine.ScheduleInfo, error) {
	return []engine.ScheduleInfo{f.schedules["nightly"]}, nil
}
func (f *fakeService) GetSchedule(_ context.Context, name string) (*engine.ScheduleInfo, error) {
	s, ok := f.schedules[name]
	if !ok {
		return nil, engine.ErrNotFound
	}
	return &s, nil
}
func (f *fakeService) PauseSchedule(context.Context, string) error  { return nil }
func (f *fakeService) ResumeSchedule(context.Context, string) error { return nil }
func (f *fakeService) DeleteSchedule(context.Context, string) error { return nil }
func (f *fakeService) Inventory(context.Context, engine.InventoryFilter) ([]engine.InventoryEntry, error) {
	return []engine.InventoryEntry{{Application: "team-a-api", Environment: "test"}, {Application: "team-b-api", Environment: "test"}}, nil
}
func (f *fakeService) Promote(context.Context, engine.PromoteRequest) (*engine.Submission, error) {
	return &engine.Submission{ID: "promo"}, nil
}
func (f *fakeService) Rollback(context.Context, engine.RollbackRequest) (*engine.Submission, error) {
	return &engine.Submission{ID: "rb"}, nil
}
func (f *fakeService) Decide(_ context.Context, id string, _ engine.DecisionRequest) error {
	f.decided = append(f.decided, id)
	return nil
}
func (f *fakeService) Freezes(context.Context, string, string) ([]governance.WindowStatus, error) {
	return nil, nil
}
func (f *fakeService) Approvals(context.Context) ([]engine.Status, error) {
	var out []engine.Status
	for _, id := range []string{"a1", "b1"} {
		out = append(out, f.statuses[id])
	}
	return out, nil
}
func (f *fakeService) Me(ctx context.Context) (*engine.Identity, error) {
	return (&engine.Engine{}).Me(ctx)
}

func (f *fakeService) Validate(context.Context, *descriptor.Descriptor) (*engine.Validation, error) {
	return &engine.Validation{Valid: true}, nil
}

func (f *fakeService) Steps(context.Context, string) ([]engine.StepRecord, error) {
	return []engine.StepRecord{{Name: "deploy"}}, nil
}

func (f *fakeService) Logs(context.Context, string, int) (*deploylog.Page, error) {
	return &deploylog.Page{Lines: []deploylog.Line{{Message: "ok"}}}, nil
}

// DORA ignora Match, come un servizio remoto: il Guard deve filtrare comunque.
func (f *fakeService) DORA(context.Context, engine.DORAFilter) (*engine.DORAReport, error) {
	return &engine.DORAReport{Environments: []engine.DORAMetrics{
		{Application: "team-a-api", Environment: "test"}, {Application: "team-b-api", Environment: "test"},
	}}, nil
}

// Workers e Activity ignorano Match, come un servizio remoto: il Guard deve filtrare comunque.
func (f *fakeService) Workers(context.Context) ([]engine.WorkerInfo, error) {
	return []engine.WorkerInfo{{ID: "w1", State: "alive", Tasks: []engine.WorkerTask{
		{ID: "a1", Application: "team-a-api", Environment: "test"}, {ID: "b1", Application: "team-b-api", Environment: "test"},
	}}}, nil
}

func (f *fakeService) Activity(context.Context, engine.ActivityFilter) (*engine.Activity, error) {
	return &engine.Activity{Running: []engine.Status{f.statuses["a1"]}, Queued: []engine.Status{f.statuses["b1"]}}, nil
}

func (f *fakeService) SubmitRelease(_ context.Context, r *descriptor.Release) (*engine.ReleaseStatus, error) {
	rs := &engine.ReleaseStatus{ID: r.ID}
	for _, rd := range r.Deploys {
		rs.Deploys = append(rs.Deploys, engine.ReleaseDeployState{Name: rd.Name, DeploymentID: r.ID + "." + rd.Name,
			Application: rd.Descriptor.Application, Environment: rd.Descriptor.Environment})
	}
	return rs, nil
}

func (f *fakeService) releases() []engine.ReleaseStatus {
	return []engine.ReleaseStatus{
		{ID: "ra", Deploys: []engine.ReleaseDeployState{{DeploymentID: "a1", Application: "team-a-api", Environment: "test"}}},
		{ID: "rab", Deploys: []engine.ReleaseDeployState{
			{DeploymentID: "a1", Application: "team-a-api", Environment: "test"}, {DeploymentID: "b1", Application: "team-b-api", Environment: "test"},
		}},
	}
}

func (f *fakeService) ReleaseStatus(_ context.Context, id string) (*engine.ReleaseStatus, error) {
	for _, rs := range f.releases() {
		if rs.ID == id {
			return &rs, nil
		}
	}
	return nil, engine.ErrNotFound
}

func (f *fakeService) ListReleases(context.Context, int) ([]engine.ReleaseStatus, error) {
	return f.releases(), nil
}

func (f *fakeService) CancelRelease(context.Context, string) error { return nil }

type recordAudit struct{ entries []audit.Entry }

func (r *recordAudit) Record(_ context.Context, e audit.Entry) error {
	r.entries = append(r.entries, e)
	return nil
}

func TestGuard(t *testing.T) {
	svc := &fakeService{
		statuses: map[string]engine.Status{
			"a1": {ID: "a1", Application: "team-a-api", Environment: "test"},
			"b1": {ID: "b1", Application: "team-b-api", Environment: "test"},
		},
		schedules: map[string]engine.ScheduleInfo{"nightly": {Name: "nightly", Application: "team-b-api", Environment: "test"}},
	}
	rec := &recordAudit{}
	g := &Guard{Next: svc, Policy: testPolicy(t), Audit: rec, Logger: discardLogger()}
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Groups: []string{"team-a"}, Method: auth.MethodOIDC})

	d := &descriptor.Descriptor{Application: "team-a-api", Environment: "test"}
	if _, err := g.Submit(alice, d); err != nil {
		t.Fatal(err)
	}
	d.Environment = "prod"
	if _, err := g.Submit(alice, d); !errors.Is(err, engine.ErrForbidden) {
		t.Fatalf("atteso ErrForbidden in prod, ricevuto %v", err)
	}
	d.DryRun = true
	if _, err := g.Submit(alice, d); err != nil {
		t.Fatalf("dry-run in prod ammesso dal ruolo tester: %v", err)
	}

	// Lettura e annullamento solo sulle proprie applicazioni.
	if _, err := g.Status(alice, "b1"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("atteso ErrForbidden, ricevuto %v", err)
	}
	if err := g.Cancel(alice, "a1"); err != nil || len(svc.cancelled) != 1 {
		t.Errorf("annullamento non eseguito: %v", err)
	}
	if list, _ := g.List(alice, engine.ListFilter{}); len(list) != 1 || list[0].ID != "a1" {
		t.Errorf("elenco non filtrato: %+v", list)
	}
	if ss, _ := g.ListSchedules(alice); len(ss) != 0 {
		t.Errorf("schedule di un altro team visibili: %+v", ss)
	}
	// Validate mostra la configurazione effettiva: richiede la lettura.
	if _, err := g.Validate(alice, &descriptor.Descriptor{Application: "team-b-api", Environment: "test"}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("validate di un altro team: atteso ErrForbidden, ricevuto %v", err)
	}
	if _, err := g.Validate(alice, &descriptor.Descriptor{Application: "team-a-api", Environment: "prod"}); err != nil {
		t.Errorf("validate in lettura: %v", err)
	}
	// Timeline, log e metriche DORA seguono il permesso di lettura.
	if _, err := g.Logs(alice, "b1", 0); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("log di un altro team: atteso ErrForbidden, ricevuto %v", err)
	}
	if _, err := g.Steps(alice, "b1"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("step di un altro team: atteso ErrForbidden, ricevuto %v", err)
	}
	if p, err := g.Logs(alice, "a1", 0); err != nil || len(p.Lines) != 1 {
		t.Errorf("log non letti: %+v %v", p, err)
	}
	if rep, _ := g.DORA(alice, engine.DORAFilter{}); len(rep.Environments) != 1 || rep.Environments[0].Application != "team-a-api" {
		t.Errorf("DORA non filtrate: %+v", rep)
	}

	// Release: permessi su ogni deploy, lettura delle dipendenze esterne, rollback se richiesto.
	item := func(name, app, env string, after ...string) descriptor.ReleaseDeploy {
		return descriptor.ReleaseDeploy{Name: name, Descriptor: &descriptor.Descriptor{Application: app, Environment: env, After: after}}
	}
	if _, err := g.SubmitRelease(alice, &descriptor.Release{ID: "r1", Deploys: []descriptor.ReleaseDeploy{item("api", "team-a-api", "test"), item("web", "team-b-api", "test")}}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("release con un deploy di un altro team: %v", err)
	}
	if _, err := g.SubmitRelease(alice, &descriptor.Release{ID: "r2", Deploys: []descriptor.ReleaseDeploy{item("api", "team-a-api", "test", "b1")}}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("dipendenza da un deploy non leggibile: %v", err)
	}
	if _, err := g.SubmitRelease(alice, &descriptor.Release{ID: "r3", OnFailure: descriptor.ReleaseOnFailureRollback, Deploys: []descriptor.ReleaseDeploy{item("api", "team-a-api", "test", "a1")}}); err != nil {
		t.Errorf("release ammessa: %v", err)
	}
	if _, err := g.ReleaseStatus(alice, "rab"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("release con deploy non leggibili: %v", err)
	}
	if list, _ := g.ListReleases(alice, 0); len(list) != 1 || list[0].ID != "ra" {
		t.Errorf("elenco release non filtrato: %+v", list)
	}
	d.Environment, d.DryRun, d.After = "test", false, []string{"b1"}
	if _, err := g.Submit(alice, d); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("after su un deploy non leggibile: %v", err)
	}

	// Attività: solo i deploy leggibili.
	if a, err := g.Activity(alice, engine.ActivityFilter{}); err != nil || len(a.Running) != 1 || len(a.Queued) != 0 {
		t.Errorf("attività non filtrata: %+v %v", a, err)
	}
	// Worker: platform.read, che il ruolo deployer non ha.
	if _, err := g.Workers(alice); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("worker senza platform.read: atteso ErrForbidden, ricevuto %v", err)
	}

	// Una schedule con lo stesso nome di un'altra applicazione non può essere sovrascritta.
	op := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "ops", Method: auth.MethodOIDC})
	cron := &descriptor.Descriptor{Application: "team-a-api", Environment: "test", Schedule: &descriptor.Schedule{Cron: "0 2 * * *", Name: "nightly"}}
	if _, err := g.Submit(op, cron); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("atteso ErrForbidden (schedule.write su team-b-api), ricevuto %v", err)
	}
	cron.Schedule.Name = "team-a-nightly"
	if _, err := g.Submit(op, cron); err != nil {
		t.Errorf("schedule nuova non creata: %v", err)
	}
	// operator ha platform.read (vale qualunque binding): vede i worker, ma dei workflow in
	// esecuzione solo quelli che può leggere.
	if ws, err := g.Workers(op); err != nil || len(ws) != 1 || len(ws[0].Tasks) != 1 || ws[0].Tasks[0].ID != "a1" || ws[0].HiddenTasks != 1 {
		t.Errorf("worker: %+v %v", ws, err)
	}

	// Senza identità nel context tutto è negato.
	if _, err := g.Status(context.Background(), "a1"); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("atteso ErrForbidden senza identità, ricevuto %v", err)
	}

	// Audit: azioni eseguite e negate, con l'identità.
	var allowed, denied int
	for _, e := range rec.entries {
		switch e.Outcome {
		case audit.OutcomeAllowed:
			allowed++
		case audit.OutcomeDenied:
			denied++
		}
	}
	if allowed != 5 || denied != 12 {
		t.Errorf("voci di audit inattese (allowed=%d denied=%d): %+v", allowed, denied, rec.entries)
	}
	if rec.entries[0].Actor != "oidc:alice" || rec.entries[0].Deployment != "new" || rec.entries[0].Action != string(DeploymentSubmit) {
		t.Errorf("prima voce inattesa: %+v", rec.entries[0])
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGuardGovernance(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"team-a"}, Role: "deployer", Applications: []string{"*"}, Environments: []string{"test", "prod"}},
		{Groups: []string{"cab"}, Role: "approver", Applications: []string{"*"}, Environments: []string{"prod"}},
		{Subjects: []string{"oidc:alice"}, Role: "approver", Applications: []string{"*"}, Environments: []string{"prod"}},
		{Subjects: []string{"oidc:oncall"}, Role: "admin", Applications: []string{"*"}, Environments: []string{"prod"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{statuses: map[string]engine.Status{
		"p1": {ID: "p1", Application: "app", Environment: "prod", RequestedBy: "oidc:alice", State: engine.StateAwaitingApproval},
	}}
	rec := &recordAudit{}
	g := &Guard{Next: svc, Policy: pol, Audit: rec, Logger: discardLogger()}
	ctxOf := func(name string, groups ...string) context.Context {
		return auth.WithPrincipal(context.Background(), &auth.Principal{Subject: name, Groups: groups, Method: auth.MethodOIDC})
	}
	alice, bob, carl := ctxOf("alice", "team-a"), ctxOf("bob", "cab"), ctxOf("carl", "team-b")

	// Chi ha richiesto il deploy non può approvarlo, anche se è approvatore.
	if err := g.Decide(alice, "p1", engine.DecisionRequest{Approve: true}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("auto-approvazione: atteso ErrForbidden, ricevuto %v", err)
	}
	// Può però ritirarlo respingendolo.
	if err := g.Decide(alice, "p1", engine.DecisionRequest{Approve: false, Comment: "ritirato"}); err != nil {
		t.Errorf("rifiuto del richiedente: %v", err)
	}
	if err := g.Decide(bob, "p1", engine.DecisionRequest{Approve: true}); err != nil || len(svc.decided) != 2 {
		t.Errorf("approvazione del cab: %v", err)
	}
	if err := g.Decide(carl, "p1", engine.DecisionRequest{Approve: true}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("approvazione senza ruolo: atteso ErrForbidden, ricevuto %v", err)
	}

	// La promozione richiede lettura sull'origine e submit sulla destinazione.
	if _, err := g.Promote(alice, engine.PromoteRequest{Application: "app", From: "test", To: "prod"}); err != nil {
		t.Errorf("promozione: %v", err)
	}
	if _, err := g.Promote(bob, engine.PromoteRequest{Application: "app", From: "test", To: "prod"}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("promozione da approvatore: atteso ErrForbidden, ricevuto %v", err)
	}
	if _, err := g.Rollback(alice, engine.RollbackRequest{Application: "app", Environment: "prod"}); err != nil {
		t.Errorf("rollback del deployer: %v", err)
	}
	if _, err := g.Rollback(bob, engine.RollbackRequest{Application: "app", Environment: "prod"}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("rollback dell'approvatore: atteso ErrForbidden, ricevuto %v", err)
	}

	// La deroga al freeze richiede un permesso a parte.
	d := &descriptor.Descriptor{Application: "app", Environment: "prod", FreezeOverride: "INC-1"}
	if _, err := g.Submit(alice, d); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("deroga senza permesso: atteso ErrForbidden, ricevuto %v", err)
	}
	if _, err := g.Submit(ctxOf("oncall"), d); err != nil {
		t.Errorf("deroga dell'admin: %v", err)
	}

	// Il piano richiede la lettura.
	if _, err := g.Plan(carl, &descriptor.Descriptor{Application: "app", Environment: "prod"}); !errors.Is(err, engine.ErrForbidden) {
		t.Errorf("piano senza permessi: atteso ErrForbidden, ricevuto %v", err)
	}
	if inv, _ := g.Inventory(carl, engine.InventoryFilter{}); len(inv) != 0 {
		t.Errorf("inventario visibile senza permessi: %+v", inv)
	}

	var approveDenied, override bool
	for _, e := range rec.entries {
		approveDenied = approveDenied || (e.Action == string(ActionApprove) && e.Outcome == audit.OutcomeDenied && e.Actor == "oidc:alice")
		override = override || (e.Action == string(DeploymentSubmit) && e.Comment == "INC-1" && e.Outcome == audit.OutcomeAllowed)
	}
	if !approveDenied || !override {
		t.Errorf("voci di audit mancanti (auto-approvazione %v, deroga %v): %+v", approveDenied, override, rec.entries)
	}
}

func TestGuardMeEApprovazioni(t *testing.T) {
	pol, err := NewPolicy(nil, []Binding{
		{Groups: []string{"cab"}, Role: "approver", Applications: []string{"team-a-*"}, Environments: []string{"*"}},
		{Groups: []string{"team-a"}, Role: "deployer", Applications: []string{"team-a-*"}, Environments: []string{"test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &fakeService{statuses: map[string]engine.Status{
		"a1": {ID: "a1", Application: "team-a-api", Environment: "test", State: engine.StateAwaitingApproval, RequestedBy: "oidc:alice"},
		"b1": {ID: "b1", Application: "team-b-api", Environment: "test", State: engine.StateAwaitingApproval, RequestedBy: "oidc:bob"},
	}}
	g := &Guard{Next: svc, Policy: pol, Logger: discardLogger()}
	carol := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "carol", Groups: []string{"cab"}, Method: auth.MethodOIDC})
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "alice", Groups: []string{"cab", "team-a"}, Method: auth.MethodOIDC})

	me, err := g.Me(alice)
	if err != nil || me.Subject != "oidc:alice" || !me.AuthEnabled || len(me.Grants) != 2 || me.Grants[0].Role != "approver" ||
		!slices.Contains(me.Grants[1].Actions, string(DeploymentSubmit)) {
		t.Errorf("me: %+v %v", me, err)
	}
	// Solo i deploy che l'identità può approvare: non quelli di altri team né i propri.
	if list, _ := g.Approvals(carol); len(list) != 1 || list[0].ID != "a1" {
		t.Errorf("approvazioni di carol: %+v", list)
	}
	if list, _ := g.Approvals(alice); len(list) != 0 {
		t.Errorf("alice non può approvare il proprio deploy: %+v", list)
	}

	// Senza autenticazione tutto è consentito e Me lo dice.
	open := &Guard{Next: svc, Policy: Policy{Disabled: true}, Logger: discardLogger()}
	if me, _ := open.Me(context.Background()); me.AuthEnabled || len(me.Grants) != 1 || me.Grants[0].Role != "*" {
		t.Errorf("me senza autenticazione: %+v", me)
	}
}
