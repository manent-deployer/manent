package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/testdb"
)

// gatedStep blocca i deploy con versione "slow*" finché il test non li libera e fa fallire
// quelli con versione "*bad".
type gatedStep struct {
	mu      sync.Mutex
	release map[string]chan struct{}
}

func (*gatedStep) Name() string { return "gated" }

func (s *gatedStep) gate(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release[id] == nil {
		s.release[id] = make(chan struct{})
	}
	return s.release[id]
}

func (s *gatedStep) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	v := in.Descriptor.Artifact.Version
	if strings.HasPrefix(v, "slow") {
		select {
		case <-s.gate(in.DeploymentID):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if strings.HasSuffix(v, "bad") {
		return nil, pipeline.Permanent(errors.New("versione difettosa"))
	}
	return pipeline.Output{"version": v}, nil
}

// chainEngine avvia un worker con letture rapide delle dipendenze e il servizio completo per
// i rollback delle release.
func chainEngine(t *testing.T) (*Engine, *gatedStep) {
	t.Helper()
	step := &gatedStep{release: map[string]chan struct{}{}}
	reg, planner := testRegistry(t, artifactStep{}, step)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ChainPollInterval = 50 * time.Millisecond
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})
	w.SetService(e)
	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(5 * time.Second) })
	return e, step
}

func appVersion(app, env, version, id string) *descriptor.Descriptor {
	d := appDescriptor(env, version)
	d.ID, d.Application, d.Artifact.Name = id, app, app
	return d
}

func TestDipendenze(t *testing.T) {
	e, step := chainEngine(t)
	ctx := context.Background()
	submit := func(d *descriptor.Descriptor) *Submission {
		t.Helper()
		sub, err := e.Submit(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return sub
	}

	// B attende A, che resta in esecuzione finché non viene liberato.
	submit(appVersion("db", "test", "slow-1", "a"))
	b := appVersion("api", "test", "1.0.0", "b")
	b.After = []string{"a"}
	if sub := submit(b); sub.State != StateWaitingDependency || sub.ID != "b" {
		t.Fatalf("B non in attesa: %+v", sub)
	}
	st := waitState(t, e, "b", StateWaitingDependency, func(s *Status) bool {
		return s.After != nil && len(s.After.Dependencies) == 1 && s.After.Dependencies[0].State == StatePending
	})
	if st.After.Status != service.AfterWaiting {
		t.Errorf("attesa: %+v", st.After)
	}
	if a, err := e.Activity(ctx, ActivityFilter{}); err != nil || len(a.WaitingDependency) != 1 || a.WaitingDependency[0].ID != "b" {
		t.Errorf("attività: %+v %v", a, err)
	}
	if list, _ := e.List(ctx, ListFilter{Application: "api"}); len(list) != 1 || list[0].State != StateWaitingDependency {
		t.Errorf("elenco: %+v", list)
	}
	close(step.gate("a"))
	if _, err := e.Wait(ctx, "b"); err != nil {
		t.Fatalf("B dopo A: %v", err)
	}
	if st, _ := e.Status(ctx, "b"); st.State != StateSuccess || st.After == nil || st.After.Status != service.AfterSatisfied {
		t.Errorf("B concluso: %+v", st)
	}
	// Se le dipendenze sono già riuscite non si attende.
	c := appVersion("web", "test", "1.0.0", "c")
	c.After = []string{"a", "b"}
	if sub := submit(c); sub.State != StateEnqueued {
		t.Errorf("C doveva partire subito: %+v", sub)
	}

	// Dipendenze non valide all'accodamento.
	bad := appVersion("api", "qa", "1.0.0", "x")
	bad.After = []string{"inesistente"}
	if _, err := e.Submit(ctx, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("dipendenza inesistente: %v", err)
	}

	// Una dipendenza che fallisce: il deploy non viene eseguito.
	submit(appVersion("db", "qa", "slow-bad", "d"))
	f := appVersion("api", "qa", "1.0.0", "f")
	f.After = []string{"d"}
	submit(f)
	waitState(t, e, "f", StateWaitingDependency, nil)
	close(step.gate("d"))
	if _, err := e.Wait(ctx, "f"); err == nil || !strings.Contains(err.Error(), "dipendenze failed") {
		t.Fatalf("atteso deploy non eseguito: %v", err)
	}
	st = waitState(t, e, "f", StateDependencyFailed, nil)
	if !strings.Contains(st.Error, "d è concluso in ERROR") {
		t.Errorf("motivo: %+v", st)
	}

	// Un deploy in attesa si annulla.
	submit(appVersion("db", "prod", "slow-2", "g"))
	h := appVersion("api", "prod", "1.0.0", "h")
	h.After = []string{"g"}
	submit(h)
	if err := e.Cancel(ctx, "h"); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "h", StateCancelled, nil)
	close(step.gate("g"))
}

func TestRelease(t *testing.T) {
	e, step := chainEngine(t)
	ctx := context.Background()
	rel := func(id, policy string, deploys ...descriptor.ReleaseDeploy) *descriptor.Release {
		return &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: id, OnFailure: policy, Deploys: deploys}
	}
	item := func(name, app, version string, after ...string) descriptor.ReleaseDeploy {
		d := appVersion(app, "prod", version, "")
		return descriptor.ReleaseDeploy{Name: name, Descriptor: d, After: after}
	}
	finish := func(id string) *ReleaseStatus {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			rs, err := e.ReleaseStatus(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if Terminal(rs.State) {
				return rs
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("release %s non conclusa", id)
		return nil
	}
	states := func(rs *ReleaseStatus) string {
		var out []string
		for _, d := range rs.Deploys {
			s := d.Name + "=" + d.State
			if d.RollbackState != "" {
				s += "/rollback=" + d.RollbackState
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	}

	// Prima release: db, poi api e worker in parallelo, web dopo api.
	r1 := rel("r1", "", item("web", "web", "1.0.0", "api"), item("api", "api", "1.0.0", "db"),
		item("db", "db", "1.0.0"), item("worker", "worker", "1.0.0", "db"))
	rs, err := e.SubmitRelease(ctx, r1)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Deploys[0].Name != "db" || rs.Deploys[0].DeploymentID != "r1.db" || rs.OnFailure != descriptor.ReleaseOnFailureStop {
		t.Fatalf("release iniziale: %+v", rs)
	}
	if again, err := e.SubmitRelease(ctx, r1); err != nil || again.ID != "r1" {
		t.Fatalf("reinvio idempotente: %+v %v", again, err)
	}
	if rs = finish("r1"); rs.State != StateSuccess {
		t.Fatalf("r1: %s %s", rs.State, states(rs))
	}
	if st, _ := e.Status(ctx, "r1.web"); st.Release != "r1" || st.After == nil {
		t.Errorf("deploy della release: %+v", st)
	}

	// Seconda release: api fallisce dopo db; web (dopo api) non parte, db torna alla 1.0.0.
	step.gate("r2.db")
	r2 := rel("r2", descriptor.ReleaseOnFailureRollback, item("db", "db", "slow-2.0.0"),
		item("api", "api", "2.0.0-bad", "db"), item("web", "web", "2.0.0", "api"))
	if _, err := e.SubmitRelease(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if rs, _ := e.ReleaseStatus(ctx, "r2"); rs.State != StatePending {
		t.Errorf("r2 in corso: %+v", rs)
	}
	close(step.gate("r2.db"))
	rs = finish("r2")
	want := "db=SUCCESS/rollback=SUCCESS api=ERROR web=" // web: annullato o non eseguito
	if rs.State != StateError || !strings.HasPrefix(states(rs), want) || !strings.Contains(rs.Error, "api: ERROR") {
		t.Fatalf("r2: %s %s (%s)", rs.State, states(rs), rs.Error)
	}
	if web := rs.Deploys[2].State; web != StateCancelled && web != StateDependencyFailed {
		t.Errorf("web non doveva partire: %s", web)
	}
	if st, _ := e.Status(ctx, "r2.rollback.db"); st.Kind != KindRollback || st.Version != "1.0.0" {
		t.Errorf("rollback di db: %+v", st)
	}

	// Release non valide.
	cyc := rel("r3", "", item("a", "a", "1", "b"), item("b", "b", "1", "a"))
	if _, err := e.SubmitRelease(ctx, cyc); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ciclo: %v", err)
	}
	if list, err := e.ListReleases(ctx, 10); err != nil || len(list) != 2 || list[0].ID != "r2" {
		t.Errorf("elenco release: %+v %v", list, err)
	}
}

func TestDipendenzeEApprovazione(t *testing.T) {
	gov, err := governance.New([]governance.Rule{{Applications: []string{"*"}, Environments: []string{"prod"},
		Approvals: governance.Approvals{Required: 1, Timeout: time.Minute}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	step := &gatedStep{release: map[string]chan struct{}{}}
	reg, planner := testRegistry(t, artifactStep{}, step)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ChainPollInterval, opts.Governance = 50*time.Millisecond, gov
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Governance: gov})

	// La dipendenza è in test, il deploy in prod richiede un'approvazione: la si chiede solo
	// dopo che la dipendenza è riuscita.
	if _, err := e.Submit(alice, appVersion("db", "test", "slow-1", "dep")); err != nil {
		t.Fatal(err)
	}
	d := appVersion("api", "prod", "1.0.0", "api-prod")
	d.After = []string{"dep"}
	if sub, err := e.Submit(alice, d); err != nil || sub.State != StateWaitingDependency {
		t.Fatalf("submission: %+v %v", sub, err)
	}
	if err := e.Decide(bob, "api-prod", DecisionRequest{Approve: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("approvazione prima delle dipendenze: %v", err)
	}
	close(step.gate("dep"))
	st := waitState(t, e, "api-prod", StateAwaitingApproval, func(s *Status) bool { return s.Approval != nil })
	if st.After == nil || st.After.Status != service.AfterSatisfied {
		t.Errorf("dipendenze nello stato: %+v", st.After)
	}
	if err := e.Decide(bob, "api-prod", DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(alice, 30*time.Second)
	defer cancel()
	if _, err := e.Wait(ctx, "api-prod"); err != nil {
		t.Fatal(err)
	}
}
