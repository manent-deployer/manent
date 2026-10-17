package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/service"
	"github.com/joeg-ita/manent/internal/testdb"
)

// gatedStep blocks deploys with version "slow*" until the test releases them and fails those
// with version "*bad".
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

// chainEngine starts a worker with fast dependency reads and the full service for release rollbacks.
func chainEngine(t *testing.T) (*Engine, *gatedStep) {
	return chainEngineEvery(t, 50*time.Millisecond)
}

// chainEngineEvery is chainEngine with the given read interval.
func chainEngineEvery(t *testing.T, poll time.Duration) (*Engine, *gatedStep) {
	t.Helper()
	step := &gatedStep{release: map[string]chan struct{}{}}
	reg, planner := testRegistry(t, artifactStep{}, step)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.ChainPollInterval = poll
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

	// B waits for A, which keeps running until released.
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
	// If the dependencies already succeeded there is no wait.
	c := appVersion("web", "test", "1.0.0", "c")
	c.After = []string{"a", "b"}
	if sub := submit(c); sub.State != StateEnqueued {
		t.Errorf("C doveva partire subito: %+v", sub)
	}

	// Invalid dependencies at submission.
	bad := appVersion("api", "qa", "1.0.0", "x")
	bad.After = []string{"inesistente"}
	if _, err := e.Submit(ctx, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("dipendenza inesistente: %v", err)
	}

	// A dependency that fails: the deploy does not run.
	submit(appVersion("db", "qa", "slow-bad", "d"))
	f := appVersion("api", "qa", "1.0.0", "f")
	f.After = []string{"d"}
	submit(f)
	waitState(t, e, "f", StateWaitingDependency, nil)
	close(step.gate("d"))
	if _, err := e.Wait(ctx, "f"); err == nil || !strings.Contains(err.Error(), "dependencies failed") {
		t.Fatalf("atteso deploy non eseguito: %v", err)
	}
	st = waitState(t, e, "f", StateDependencyFailed, nil)
	if !strings.Contains(st.Error, "d ended in ERROR") {
		t.Errorf("motivo: %+v", st)
	}

	// A waiting deploy can be cancelled.
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

	// First release: db, then api and worker in parallel, web after api.
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

	// Second release: api fails after db; web (after api) does not start, db goes back to 1.0.0.
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
	want := "db=SUCCESS/rollback=SUCCESS api=ERROR web=" // web: cancelled or not run
	if rs.State != StateError || !strings.HasPrefix(states(rs), want) || !strings.Contains(rs.Error, "api: ERROR") {
		t.Fatalf("r2: %s %s (%s)", rs.State, states(rs), rs.Error)
	}
	if web := rs.Deploys[2].State; web != StateCancelled && web != StateDependencyFailed {
		t.Errorf("web non doveva partire: %s", web)
	}
	if st, _ := e.Status(ctx, "r2.rollback.db"); st.Kind != KindRollback || st.Version != "1.0.0" {
		t.Errorf("rollback di db: %+v", st)
	}

	// Invalid releases.
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

	// The dependency is in test, the prod deploy requires an approval: it is asked for only after
	// the dependency succeeded.
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

// With a one-hour read interval, waits and releases progress only thanks to the messages of
// whoever ends: successful deploy, cancelled deploy, release deploy.
func TestCateneSvegliateDaiMessaggi(t *testing.T) {
	e, step := chainEngineEvery(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	submit := func(d *descriptor.Descriptor, after ...string) {
		t.Helper()
		d.After = after
		if _, err := e.Submit(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	submit(appVersion("db", "test", "slow-1", "a"))
	submit(appVersion("api", "test", "1.0.0", "b"), "a")
	// The dependency may still be queued: its start does not wake the wait.
	waitState(t, e, "b", StateWaitingDependency, func(s *Status) bool {
		return s.After != nil && len(s.After.Dependencies) == 1
	})
	close(step.gate("a"))
	if _, err := e.Wait(ctx, "b"); err != nil {
		t.Fatalf("B non svegliato dalla conclusione di A: %v", err)
	}

	submit(appVersion("db", "test", "slow-2", "c"))
	submit(appVersion("api", "test", "2.0.0", "d"), "c")
	// The dependency may still be queued: its start does not wake the wait.
	waitState(t, e, "d", StateWaitingDependency, func(s *Status) bool {
		return s.After != nil && len(s.After.Dependencies) == 1
	})
	if err := e.Cancel(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	if st := waitState(t, e, "d", StateDependencyFailed, nil); !strings.Contains(st.Error, "CANCELLED") {
		t.Errorf("motivo: %q", st.Error)
	}

	r := &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: "rel", Deploys: []descriptor.ReleaseDeploy{
		{Name: "db", Descriptor: appVersion("db", "prod", "3.0.0", "")},
		{Name: "api", Descriptor: appVersion("api", "prod", "3.0.0", ""), After: []string{"db"}},
	}}
	if _, err := e.SubmitRelease(ctx, r); err != nil {
		t.Fatal(err)
	}
	for {
		rs, err := e.ReleaseStatus(ctx, "rel")
		if err != nil {
			t.Fatal(err)
		}
		if rs.State == StateSuccess {
			break
		}
		if Terminal(rs.State) || ctx.Err() != nil {
			t.Fatalf("release: %+v", rs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A release cancelled while a deploy is running also cancels the unfinished deploys.
func TestReleaseAnnullata(t *testing.T) {
	e, _ := chainEngine(t)
	ctx := context.Background()
	r := &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: "rel", Deploys: []descriptor.ReleaseDeploy{
		{Name: "db", Descriptor: appVersion("db", "prod", "slow-1", "")},
		{Name: "api", Descriptor: appVersion("api", "prod", "1.0.0", ""), After: []string{"db"}},
	}}
	if _, err := e.SubmitRelease(ctx, r); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "rel.api", StateWaitingDependency, nil)
	if err := e.CancelRelease(ctx, "rel"); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "rel.db", StateCancelled, nil)
	waitState(t, e, "rel.api", StateCancelled, nil)
	if rs, err := e.ReleaseStatus(ctx, "rel"); err != nil || rs.State != StateCancelled {
		t.Errorf("release: %+v %v", rs, err)
	}
}

// The inventory read several times by the same engine follows the deploys ended after the
// first read (incremental read), including those running at read time.
func TestInventarioIncrementale(t *testing.T) {
	e, step := chainEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	current := func() (string, string) {
		t.Helper()
		inv, err := e.Inventory(ctx, InventoryFilter{Application: "db"})
		if err != nil {
			t.Fatal(err)
		}
		if len(inv) != 1 {
			return "", ""
		}
		cur, fail := "", ""
		if inv[0].Current != nil {
			cur = inv[0].Current.Version
		}
		if inv[0].LastFailure != nil {
			fail = inv[0].LastFailure.Version
		}
		return cur, fail
	}
	run := func(version, id string) {
		t.Helper()
		if _, err := e.Submit(ctx, appVersion("db", "test", version, id)); err != nil {
			t.Fatal(err)
		}
		_, _ = e.Wait(ctx, id)
	}

	if cur, _ := current(); cur != "" {
		t.Fatalf("inventario vuoto atteso: %q", cur)
	}
	run("1.0.0", "i1")
	if cur, fail := current(); cur != "1.0.0" || fail != "" {
		t.Errorf("dopo i1: %q %q", cur, fail)
	}
	// i2 is running at read time and ends afterwards.
	if _, err := e.Submit(ctx, appVersion("db", "test", "slow-2.0.0", "i2")); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "i2", StatePending, nil)
	if cur, _ := current(); cur != "1.0.0" {
		t.Errorf("i2 in corso: %q", cur)
	}
	close(step.gate("i2"))
	_, _ = e.Wait(ctx, "i2")
	if cur, fail := current(); cur != "slow-2.0.0" || fail != "" {
		t.Errorf("dopo i2: %q %q", cur, fail)
	}
	run("3.0.0-bad", "i3")
	if cur, fail := current(); cur != "slow-2.0.0" || fail != "3.0.0-bad" {
		t.Errorf("dopo i3 fallito: %q %q", cur, fail)
	}
	run("4.0.0", "i4")
	if cur, fail := current(); cur != "4.0.0" || fail != "" {
		t.Errorf("dopo i4: %q %q", cur, fail)
	}
}

// List and cursor go through several scan pages (200 workflows) without losing or repeating
// deploys, even with many workflows created in the same millisecond.
func TestElencoPerChiave(t *testing.T) {
	e, _ := chainEngine(t)
	const n = 450
	for i := range n {
		req := Request{Descriptor: *appVersion("app", "test", "1.0.0", "")}
		// A queue without workers: deploys stay queued.
		if _, err := dbos.Enqueue[Result](e.client, "ferma", workflowName, req, dbos.WithEnqueueWorkflowID(fmt.Sprintf("k%03d", i)),
			dbos.WithEnqueueAttributes(attributes(req)), dbos.WithEnqueueQueuePartitionKey("app/test")); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	all, err := e.List(ctx, ListFilter{Limit: 1000})
	if err != nil || len(all) != n {
		t.Fatalf("elenco: %d %v", len(all), err)
	}
	seen := map[string]bool{}
	for i, s := range all {
		if seen[s.ID] {
			t.Fatalf("deploy ripetuto: %s", s.ID)
		}
		seen[s.ID] = true
		if i > 0 && (s.CreatedAt.After(all[i-1].CreatedAt) || (s.CreatedAt.Equal(all[i-1].CreatedAt) && s.ID > all[i-1].ID)) {
			t.Fatalf("ordine: %s dopo %s", s.ID, all[i-1].ID)
		}
	}
	var paged []string
	cursor := ""
	for {
		page, err := e.List(ctx, ListFilter{Limit: 70, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			paged = append(paged, s.ID)
		}
		if len(page) < 70 {
			break
		}
		cursor = page[len(page)-1].ID
	}
	// The order is deterministic (creation time, then ID) even between deploys created in the
	// same millisecond: the pages reproduce the full list.
	if len(paged) != n {
		t.Fatalf("pagine: %d deploy", len(paged))
	}
	for i, id := range paged {
		if id != all[i].ID {
			t.Fatalf("pagina diversa dall'elenco alla posizione %d: %s, atteso %s", i, id, all[i].ID)
		}
	}
	if inv, err := e.Inventory(ctx, InventoryFilter{}); err != nil || len(inv) != 0 {
		t.Errorf("inventario con soli deploy in coda: %+v %v", inv, err)
	}
}

// A group of workflows with the same creation time larger than a scan page is read entirely,
// in ID order.
func TestElencoGruppoStessaData(t *testing.T) {
	url := testdb.URL(t)
	reg, _ := testRegistry(t, artifactStep{})
	w := startWorker(t, workerOpts(url, "w1", reg))
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Logger: discard})
	enqueue := func(id string) {
		t.Helper()
		req := Request{Descriptor: *appVersion("app", "test", "1.0.0", "")}
		if _, err := dbos.Enqueue[Result](e.client, "ferma", workflowName, req, dbos.WithEnqueueWorkflowID(id),
			dbos.WithEnqueueAttributes(attributes(req)), dbos.WithEnqueueQueuePartitionKey("app/test")); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 250 {
		enqueue(fmt.Sprintf("g%03d", i))
	}
	// The same 250 workflows created in the same millisecond.
	driver, dsn, table := "sqlite", strings.TrimPrefix(url, "sqlite:"), "workflow_status"
	if testdb.Postgres() {
		driver, dsn, table = "pgx", url, "dbos.workflow_status"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE " + table + " SET created_at = 1700000000000 WHERE workflow_uuid LIKE 'g%'"); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		enqueue(fmt.Sprintf("n%03d", i))
	}
	ctx := context.Background()
	all, err := e.List(ctx, ListFilter{Limit: 1000})
	if err != nil || len(all) != 280 {
		t.Fatalf("elenco: %d %v", len(all), err)
	}
	if all[30].ID != "g249" || all[279].ID != "g000" {
		t.Errorf("gruppo non in ordine di ID: %s … %s", all[30].ID, all[279].ID)
	}
	var paged []string
	for cursor := ""; ; {
		page, err := e.List(ctx, ListFilter{Limit: 70, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			paged = append(paged, s.ID)
		}
		if len(page) < 70 {
			break
		}
		cursor = page[len(page)-1].ID
	}
	if len(paged) != len(all) {
		t.Fatalf("pagine: %d deploy", len(paged))
	}
	for i := range paged {
		if paged[i] != all[i].ID {
			t.Fatalf("posizione %d: %s, atteso %s", i, paged[i], all[i].ID)
		}
	}
}
