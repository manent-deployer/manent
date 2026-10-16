package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/rollback"
	"github.com/joeg-ita/manent/internal/testdb"
)

// fakeChat records the sent messages; updated messages carry the reference of the previous send.
type fakeChat struct {
	mu   sync.Mutex
	sent []string // "<destination>:<event>[:update]"
	msgs []chat.Message
}

func (f *fakeChat) Wants(route, event string) bool {
	return route != "solo-approvazioni" || chat.Category(event) == chat.CategoryApproval
}

func (f *fakeChat) Send(_ context.Context, route string, msg chat.Message, prev *chat.Ref) (*chat.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := route + ":" + msg.Event
	if prev != nil {
		entry += ":update"
	}
	f.sent = append(f.sent, entry)
	f.msgs = append(f.msgs, msg)
	return &chat.Ref{Channel: "C1", TS: fmt.Sprint(len(f.sent))}, nil
}

func (f *fakeChat) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// fakeChange is an in-memory ITSM: the test sets the change approval.
type fakeChange struct {
	mu       sync.Mutex
	tickets  map[string]*change.Ticket // per deploy
	approval string
	calls    []string
	failOpen bool
}

func newFakeChange() *fakeChange {
	return &fakeChange{tickets: map[string]*change.Ticket{}, approval: change.ApprovalPending}
}

func (f *fakeChange) Name() string { return "fake" }

func (f *fakeChange) Open(_ context.Context, r change.Request) (change.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "open:"+r.DeploymentID)
	if f.failOpen {
		return change.Ticket{}, fmt.Errorf("%w: 403 forbidden", change.ErrPermanent)
	}
	if t, ok := f.tickets[r.DeploymentID]; ok {
		return *t, nil
	}
	t := &change.Ticket{ID: "sys-" + r.DeploymentID, Number: fmt.Sprintf("CHG%04d", len(f.tickets)+1), URL: "https://itsm.example.com/chg", Approval: f.approval}
	f.tickets[r.DeploymentID] = t
	return *t, nil
}

func (f *fakeChange) Refresh(_ context.Context, t change.Ticket) (change.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "refresh:"+t.Number)
	t.Approval, t.State = f.approval, "assess"
	return t, nil
}

func (f *fakeChange) Start(_ context.Context, t change.Ticket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start:"+t.Number)
	return nil
}

func (f *fakeChange) Close(_ context.Context, t change.Ticket, o change.Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("close:%s:%v", t.Number, o.Success))
	if t.ID == "" {
		return errors.New("ticket senza ID")
	}
	return nil
}

func (f *fakeChange) setApproval(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approval = a
}

func (f *fakeChange) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// staticRoutes chooses the destinations per environment.
type staticRoutes map[string][]string

func (s staticRoutes) Match(_, environment string, dryRun bool) []string {
	if dryRun {
		return nil
	}
	return s[environment]
}

func integrationEngine(t *testing.T, rules []governance.Rule, ch *fakeChange, ct *fakeChat) *Engine {
	t.Helper()
	gov, err := governance.New(rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Governance = gov
	opts.Chat = ct
	opts.Change = ch
	opts.ChangePoll = 20 * time.Millisecond
	opts.PublicURL = "https://manent.example.com"
	w := startWorker(t, opts)
	return NewService(ServiceOptions{
		Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		Governance: gov, Rollback: rollback.NewPolicy(config.Default(), []string{"plugin"}),
		Notifications: staticRoutes{"prod": {"prod-slack", "solo-approvazioni"}},
	})
}

func TestNotificheEChange(t *testing.T) {
	ch, ct := newFakeChange(), &fakeChat{}
	e := integrationEngine(t, []governance.Rule{{Environments: []string{"prod"}, Applications: []string{"*"}, Change: &governance.Change{}}}, ch, ct)

	id := deploy(t, e, alice, appDescriptor("prod", "1.0.0"))
	st, err := e.Status(alice, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "SUCCESS" || st.Change == nil || st.Change.Number != "CHG0001" || st.Change.URL == "" {
		t.Fatalf("stato: %s, change %+v", st.State, st.Change)
	}
	if got := ch.log(); !slices.Equal(got, []string{"open:" + id, "start:CHG0001", "close:CHG0001:true"}) {
		t.Errorf("chiamate al sistema ITSM: %v", got)
	}
	// Start and outcome go to the deploy channel; the outcome updates the start message.
	if got := ct.events(); !slices.Equal(got, []string{"prod-slack:started", "prod-slack:succeeded:update"}) {
		t.Errorf("notifiche: %v", got)
	}
	last := ct.msgs[len(ct.msgs)-1]
	if last.Change == nil || last.Change.Number != "CHG0001" || last.Version != "1.0.0" || last.RequestedBy != "oidc:alice" {
		t.Errorf("messaggio finale: %+v", last)
	}

	// A failed deploy closes the change as unsuccessful and notifies the error.
	failed, err := e.Submit(alice, appDescriptor("prod", "2.0.0-broken"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, failed.ID); err == nil {
		t.Fatal("atteso errore")
	}
	if got := ch.log(); got[len(got)-1] != "close:CHG0002:false" {
		t.Errorf("chiusura del change fallito: %v", got)
	}
	if last := ct.msgs[len(ct.msgs)-1]; last.Event != chat.EventFailed || !strings.Contains(last.Error, "release difettosa") {
		t.Errorf("notifica del fallimento: %+v", last)
	}

	// A dry-run opens no change and sends no notification; neither does an environment without rules.
	dry := appDescriptor("prod", "3.0.0")
	dry.DryRun = true
	deploy(t, e, alice, dry)
	deploy(t, e, alice, appDescriptor("test", "3.0.0"))
	if n := len(ch.log()); n != 6 {
		t.Errorf("chiamate ITSM per dry-run o ambienti senza regola: %v", ch.log())
	}
	if n := len(ct.events()); n != 4 {
		t.Errorf("notifiche per dry-run o ambienti senza destinazioni: %v", ct.events())
	}

	// Without a ticket the deploy does not start.
	ch.mu.Lock()
	ch.failOpen = true
	ch.mu.Unlock()
	blocked, err := e.Submit(alice, appDescriptor("prod", "4.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, blocked.ID); err == nil || !strings.Contains(err.Error(), "change management") {
		t.Errorf("deploy senza change: %v", err)
	}
}

func TestApprovazioneDelChange(t *testing.T) {
	ch, ct := newFakeChange(), &fakeChat{}
	rules := []governance.Rule{{
		Environments: []string{"prod"}, Applications: []string{"*"},
		Approvals: governance.Approvals{Required: 1, Timeout: time.Hour},
		Change:    &governance.Change{Approval: true, Timeout: time.Hour},
	}}
	e := integrationEngine(t, rules, ch, ct)

	sub, err := e.Submit(alice, appDescriptor("prod", "1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	st := waitState(t, e, sub.ID, StateAwaitingApproval, func(s *Status) bool { return s.Approval != nil && s.Approval.Change != nil })
	if st.Approval.Change.Approval != change.ApprovalPending || st.Change == nil || st.Change.Number != "CHG0001" {
		t.Fatalf("approvazione: %+v, change %+v", st.Approval, st.Change)
	}
	// manent's approval alone is not enough: the change is needed too.
	if err := e.Decide(bob, sub.ID, DecisionRequest{Approve: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if st, _ := e.Status(alice, sub.ID); st.State != StateAwaitingApproval {
		t.Fatalf("deploy partito senza l'approvazione del change: %s", st.State)
	}
	ch.setApproval(change.ApprovalApproved)
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := ch.log(); !slices.Contains(got, "start:CHG0001") || got[len(got)-1] != "close:CHG0001:true" {
		t.Errorf("il deploy non ha usato il change approvato: %v", got)
	}
	ev := ct.events()
	for _, want := range []string{"prod-slack:approval_requested", "solo-approvazioni:approval_requested", "prod-slack:approval_approved:update", "solo-approvazioni:approval_approved:update", "prod-slack:succeeded:update"} {
		if !slices.Contains(ev, want) {
			t.Errorf("manca la notifica %s in %v", want, ev)
		}
	}
	if slices.Contains(ev, "solo-approvazioni:started") {
		t.Error("la destinazione delle sole approvazioni ha ricevuto l'avvio del deploy")
	}

	// A change rejected in the ITSM rejects the deploy.
	ch.setApproval(change.ApprovalRejected)
	sub2, err := e.Submit(alice, appDescriptor("prod", "2.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	st = waitState(t, e, sub2.ID, StateRejected, nil)
	if n := len(st.Approval.Decisions); n != 1 || !strings.HasPrefix(st.Approval.Decisions[0].Approver, "fake:CHG") {
		t.Errorf("decisione del sistema ITSM: %+v", st.Approval.Decisions)
	}
}

func TestSoloApprovazioneDelChange(t *testing.T) {
	ch := newFakeChange()
	e := integrationEngine(t, []governance.Rule{{
		Environments: []string{"prod"}, Applications: []string{"*"}, Change: &governance.Change{Approval: true, Timeout: time.Hour},
	}}, ch, &fakeChat{})
	// A change already approved at creation (e.g. standard change) does not make it wait.
	ch.setApproval(change.ApprovalApproved)
	sub, err := e.Submit(alice, appDescriptor("prod", "1.0.0"))
	if err != nil || sub.State != StateAwaitingApproval {
		t.Fatalf("accodamento: %+v, %v", sub, err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
}
