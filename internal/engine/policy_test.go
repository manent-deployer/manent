package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/cluster"
	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/governance"
	"github.com/manent-deployer/manent/internal/policy"
	"github.com/manent-deployer/manent/internal/rollback"
	"github.com/manent-deployer/manent/internal/service"
	"github.com/manent-deployer/manent/internal/testdb"
)

// switchGov is a governance policy that tests change at runtime, as a new revision or a
// runtime freeze would.
type switchGov struct {
	mu    sync.Mutex
	p     *governance.Policy
	deny  string
	calls []string
}

func newSwitchGov(t *testing.T) *switchGov {
	s := &switchGov{}
	s.set(t, nil, nil)
	return s
}

func (s *switchGov) set(t *testing.T, rules []governance.Rule, windows []governance.Window) {
	t.Helper()
	p, err := governance.New(rules, windows)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

func (s *switchGov) setDeny(msg string) {
	s.mu.Lock()
	s.deny = msg
	s.mu.Unlock()
}

func (s *switchGov) Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error) {
	s.mu.Lock()
	p, deny := s.p, s.deny
	s.calls = append(s.calls, in.Phase)
	s.mu.Unlock()
	dec, err := p.Evaluate(ctx, in)
	if deny != "" {
		dec.Deny = append(dec.Deny, governance.Denial{Code: "test", Message: deny})
	}
	return dec, err
}

func (s *switchGov) Windows(application, environment string, t time.Time) []governance.WindowStatus {
	s.mu.Lock()
	p := s.p
	s.mu.Unlock()
	return p.Windows(application, environment, t)
}

func (s *switchGov) phases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// policyEngine starts worker and engine on a governance policy changed by the test.
func policyEngine(t *testing.T, gov Governance, start string) *Engine {
	t.Helper()
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Governance, opts.PolicyStart = gov, start
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{
		Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		Governance: gov, Rollback: rollback.NewPolicy(config.Default(), []string{"plugin"}),
	})
	w.SetService(e)
	return e
}

func TestPolicyNegaAlSubmitERegistraLaDecisione(t *testing.T) {
	gov := newSwitchGov(t)
	e := policyEngine(t, gov, "")
	gov.setDeny("no deploys on Friday")
	if _, err := e.Submit(alice, appDescriptor("prod", "1.0.0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "no deploys on Friday") {
		t.Fatalf("divieto della policy: %v", err)
	}
	v, err := e.Validate(alice, appDescriptor("prod", "1.0.0"), ValidateOptions{})
	if err != nil || len(v.Governance.Deny) != 1 || !strings.HasPrefix(v.Governance.Revision, "builtin:") {
		t.Fatalf("validazione con il divieto: %+v %v", v.Governance, err)
	}
	gov.setDeny("")
	id := deploy(t, e, alice, appDescriptor("prod", "1.0.0"))
	st, err := e.Status(alice, id)
	if err != nil || st.Policy == nil || !strings.HasPrefix(st.Policy.Revision, "builtin:") {
		t.Fatalf("decisione registrata nello stato: %+v %v", st.Policy, err)
	}
	steps, err := e.Steps(alice, id)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Name != "config" || !strings.HasPrefix(stepOutput(t, steps[0])["policy_revision"], "builtin:") {
		t.Errorf("revisione nell'output dello step config: %+v", steps[0])
	}
	if ph := gov.phases(); !slices.Contains(ph, governance.PhaseSubmit) || !slices.Contains(ph, governance.PhaseStart) || !slices.Contains(ph, governance.PhaseValidate) {
		t.Errorf("fasi valutate: %v", ph)
	}
}

// stepOutput reads the meta of the recorded config step.
func stepOutput(t *testing.T, s StepRecord) map[string]string {
	t.Helper()
	b, err := json.Marshal(s.Output)
	if err != nil {
		t.Fatal(err)
	}
	switch o := s.Output.(type) {
	case json.RawMessage:
		b = o
	case string:
		b = []byte(o)
	}
	var out struct {
		Meta map[string]string `json:"meta"`
	}
	_ = json.Unmarshal(b, &out)
	return out.Meta
}

func TestFreezeArrivatoDopoLAccodamento(t *testing.T) {
	gov := newSwitchGov(t)
	e := policyEngine(t, gov, "")
	d := appDescriptor("prod", "1.0.0")
	at := time.Now().Add(2 * time.Second)
	d.Schedule = &descriptor.Schedule{At: &at}
	sub, err := e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	// An emergency freeze created after the submission stops the deploy at its start.
	gov.set(t, nil, []governance.Window{{Name: "incidente", Applications: []string{"*"}, Environments: []string{"prod"},
		From: time.Now().Add(-time.Minute), To: time.Now().Add(time.Hour), Hard: true}})
	if _, err := e.Wait(alice, sub.ID); err == nil || !strings.Contains(err.Error(), `hard freeze "incidente"`) {
		t.Fatalf("deploy avviato durante il freeze: %v", err)
	}
}

func TestStartStrictObblighiCresciuti(t *testing.T) {
	gov := newSwitchGov(t)
	e := policyEngine(t, gov, PolicyStartStrict)
	d := appDescriptor("prod", "1.0.0")
	at := time.Now().Add(2 * time.Second)
	d.Schedule = &descriptor.Schedule{At: &at}
	sub, err := e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	gov.set(t, []governance.Rule{{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Approvals: governance.Approvals{Required: 1}}}, nil)
	if _, err := e.Wait(alice, sub.ID); err == nil || !strings.Contains(err.Error(), "now requires 1 approvals") {
		t.Fatalf("obblighi cresciuti con start strict: %v", err)
	}
}

func TestStepPolicyDegliStadi(t *testing.T) {
	gov := newSwitchGov(t)
	env := newWaveEngine(t, "", gov)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-policy"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	steps, err := env.e.Steps(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	for _, want := range []string{"policy@canary#0", "policy@eu-1/eu-1b#0", "policy@resto/eu-2a+us-1a#0"} {
		if !slices.Contains(names, want) {
			t.Errorf("manca lo step %s: %v", want, names)
		}
	}
	if slices.Contains(names, "now") {
		t.Errorf("le richieste con la decisione non usano lo step now: %v", names)
	}
}

func TestStadioNegatoFermaIlRollout(t *testing.T) {
	gov := &stageDeny{switchGov: newSwitchGov(t)}
	env := newWaveEngine(t, "", gov)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-negate"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Wait(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "rollout stopped at stage") || !strings.Contains(err.Error(), "us-1 closed") {
		t.Fatalf("divieto allo stadio: %v", err)
	}
}

// stageDeny denies the rollout stages on the us-1 region.
type stageDeny struct{ *switchGov }

func (s *stageDeny) Evaluate(ctx context.Context, in governance.Input) (governance.Decision, error) {
	dec, err := s.switchGov.Evaluate(ctx, in)
	if in.Phase == governance.PhaseStage && in.Region == "us-1" {
		dec.Deny = append(dec.Deny, governance.Denial{Code: "region", Message: "us-1 closed"})
	}
	return dec, err
}

func TestTickUsaLaPolicyInVigore(t *testing.T) {
	gov := newSwitchGov(t)
	e := policyEngine(t, gov, "")
	d := appDescriptor("prod", "1.0.0")
	d.Schedule = &descriptor.Schedule{Cron: "* * * * * *", Name: "ogni-secondo"}
	if _, err := e.Submit(alice, d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.DeleteSchedule(alice, "ogni-secondo") })
	// The rule arrives after the schedule's creation: the next runs ask for an approval.
	gov.set(t, []governance.Rule{{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Approvals: governance.Approvals{Required: 1}}}, nil)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		list, err := e.List(alice, ListFilter{Schedule: "ogni-secondo", State: StateAwaitingApproval})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 {
			if !slices.Contains(gov.phases(), governance.PhaseTick) {
				t.Errorf("fase tick non valutata: %v", gov.phases())
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("nessuna esecuzione della schedule in attesa di approvazione")
}

func TestPolicyOPAEndToEnd(t *testing.T) {
	ctx := context.Background()
	url := testdb.URL(t)
	store, err := cluster.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rt, err := policy.New(policy.Options{Engine: policy.EngineOPA, Store: store, Review: true, Refresh: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(url, "w1", reg)
	opts.Governance = rt
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard, Governance: rt, Policies: rt})
	w.SetService(e)

	// Default policy without rules: no obligations.
	deploy(t, e, alice, appDescriptor("prod", "1.0.0"))
	rev, err := e.PushPolicy(alice, PolicyPush{Comment: "due approvazioni in prod", Files: map[string]string{"governance.rego": `package manent.governance

import rego.v1

default approvals := null

approvals := {"required": 2, "approvers": {"subjects": ["oidc:carol", "oidc:dave"]}} if input.environment == "prod"

decision := {"rule": "prod", "approvals": approvals}
`}})
	if err != nil {
		t.Fatal(err)
	}
	imp, err := e.PolicyImpact(alice, rev.Revision, time.Hour)
	if err != nil || imp.Evaluated != 1 || len(imp.Changes) != 1 || !strings.Contains(imp.Changes[0].After, "approvals=2") {
		t.Fatalf("impatto della revisione: %+v %v", imp, err)
	}
	ev, err := e.EvaluatePolicy(alice, rev.Revision, PolicyEvaluationRequest{Descriptor: appDescriptor("prod", "2.0.0")})
	if err != nil || ev.Decision.Approvals.RequiredOrZero() != 2 || ev.Input.Phase != governance.PhaseSubmit {
		t.Fatalf("valutazione della bozza: %+v %v", ev, err)
	}
	if err := e.ApprovePolicy(alice, rev.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("approvazione dell'autore: %v", err)
	}
	if err := e.ApprovePolicy(bob, rev.Revision); err != nil {
		t.Fatal(err)
	}
	sub, err := e.Submit(alice, appDescriptor("prod", "2.0.0"))
	if err != nil || sub.State != StateAwaitingApproval {
		t.Fatalf("deploy con la nuova revisione: %+v %v", sub, err)
	}
	var st *Status
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if st, err = e.Status(alice, sub.ID); err != nil || st.Approval != nil {
			break
		}
	}
	if err != nil || st.Approval == nil || st.Approval.Required != 2 || st.Approval.Approvers == nil || st.Policy == nil || !strings.HasPrefix(st.Policy.Revision, "1:") {
		t.Fatalf("approvazione richiesta dalla policy: %+v %v", st, err)
	}
	if err := e.Cancel(alice, sub.ID); err != nil {
		t.Fatal(err)
	}

	// Runtime freeze: it blocks at once, without restarting anything.
	if _, err := e.CreateFreeze(carol, FreezeRequest{Name: "a b", Applications: []string{"*"}, Environments: []string{"prod"}, To: time.Now().Add(time.Hour)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nome non valido: %v", err)
	}
	if _, err := e.CreateFreeze(carol, FreezeRequest{Name: "passato", Applications: []string{"*"}, Environments: []string{"prod"}, From: time.Now().Add(-2 * time.Hour), To: time.Now().Add(-time.Hour)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("freeze nel passato: %v", err)
	}
	fw, err := e.CreateFreeze(carol, FreezeRequest{Name: "incidente-7", Reason: "incidente", Applications: []string{"*"}, Environments: []string{"test"}, To: time.Now().Add(time.Hour)})
	if err != nil || !fw.Active || fw.CreatedBy != "oidc:carol" {
		t.Fatalf("freeze creato: %+v %v", fw, err)
	}
	if _, err := e.Submit(alice, appDescriptor("test", "2.0.0")); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), `freeze "incidente-7"`) {
		t.Fatalf("deploy durante il freeze a runtime: %v", err)
	}
	if err := e.DeleteFreeze(carol, "incidente-7"); err != nil {
		t.Fatal(err)
	}
	deploy(t, e, alice, appDescriptor("test", "2.0.0"))
	list, err := e.Policies(alice)
	if err != nil || len(list.Revisions) != 1 || list.Revisions[0].Status != "active" || list.Revisions[0].ReviewedBy != "oidc:bob" {
		t.Fatalf("revisioni: %+v %v", list, err)
	}
}

func TestFreezeChiusoInAnticipoRiprendeIlRollout(t *testing.T) {
	prev := freezeRecheck
	freezeRecheck = 300 * time.Millisecond
	t.Cleanup(func() { freezeRecheck = prev })
	gov := newSwitchGov(t)
	gov.set(t, nil, []governance.Window{{Name: "us-incidente", Applications: []string{"*"}, Environments: []string{"*"},
		Regions: []string{"us-1"}, From: time.Now().Add(-time.Minute), To: time.Now().Add(time.Hour)}})
	env := newWaveEngine(t, "", gov)
	ctx := context.Background()
	sub, err := env.e.Submit(ctx, waveDescriptor("ondate-freeze-chiuso"))
	if err != nil {
		t.Fatal(err)
	}
	waitRollout(t, env.e, sub.ID, service.RolloutPausedFreeze)
	// The window, one hour long, is closed at runtime: the rollout resumes at the next check.
	gov.set(t, nil, nil)
	done := make(chan error, 1)
	go func() { _, err := env.e.Wait(ctx, sub.ID); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("il rollout non è ripartito dopo la chiusura del freeze")
	}
}
