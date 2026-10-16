package governance

import (
	"testing"
	"time"
)

func TestRegolaPrimaCorrispondenza(t *testing.T) {
	p, err := New([]Rule{
		{Name: "pagamenti", Applications: []string{"pay-*"}, Environments: []string{"prod"}, Approvals: Approvals{Required: 2}},
		{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Requires: []string{"preprod"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Rule("pay-api", "prod"); r.Name != "pagamenti" || r.Approvals.Timeout != DefaultApprovalTimeout {
		t.Errorf("regola inattesa: %+v", r)
	}
	if r := p.Rule("shop", "prod"); r.Name != "prod" {
		t.Errorf("regola inattesa: %+v", r)
	}
	if r := p.Rule("shop", "test"); r.Name != "" {
		t.Errorf("nessuna regola attesa per test: %+v", r)
	}
}

func TestFinestraRicorrente(t *testing.T) {
	// Every Friday from 18:00 for 62 hours (until Monday at 8:00), Rome time.
	p, err := New(nil, []Window{{Name: "weekend", Applications: []string{"*"}, Environments: []string{"prod"},
		Cron: "0 18 * * FRI", Duration: 62 * time.Hour, Timezone: "Europe/Rome"}})
	if err != nil {
		t.Fatal(err)
	}
	rome, _ := time.LoadLocation("Europe/Rome")
	cases := map[time.Time]bool{
		time.Date(2026, 10, 9, 17, 59, 0, 0, rome): false, // Friday before 18:00
		time.Date(2026, 10, 9, 18, 0, 0, 0, rome):  true,
		time.Date(2026, 10, 11, 12, 0, 0, 0, rome): true, // Sunday
		time.Date(2026, 10, 12, 7, 59, 0, 0, rome): true,
		time.Date(2026, 10, 12, 8, 0, 0, 0, rome):  false, // Monday at 8:00
		time.Date(2026, 10, 14, 12, 0, 0, 0, rome): false,
	}
	for at, want := range cases {
		if got := p.Frozen("app", "prod", at) != nil; got != want {
			t.Errorf("%s: freeze %v, atteso %v", at, got, want)
		}
	}
	if p.Frozen("app", "test", time.Date(2026, 10, 10, 12, 0, 0, 0, rome)) != nil {
		t.Error("la finestra non riguarda test")
	}
	ws := p.Windows("", "", time.Date(2026, 10, 14, 12, 0, 0, 0, rome))
	if len(ws) != 1 || ws[0].Active || !ws[0].Start.Equal(time.Date(2026, 10, 16, 18, 0, 0, 0, rome)) {
		t.Errorf("prossima occorrenza inattesa: %+v", ws)
	}
}

func TestNewErrori(t *testing.T) {
	now := time.Now()
	for name, w := range map[string]Window{
		"senza nome":        {Applications: []string{"*"}, Environments: []string{"*"}, From: now, To: now.Add(time.Hour)},
		"to prima di from":  {Name: "x", Applications: []string{"*"}, Environments: []string{"*"}, From: now, To: now.Add(-time.Hour)},
		"cron senza durata": {Name: "x", Applications: []string{"*"}, Environments: []string{"*"}, Cron: "0 18 * * FRI"},
		"cron non valido":   {Name: "x", Applications: []string{"*"}, Environments: []string{"*"}, Cron: "ogni venerdì", Duration: time.Hour},
		"cron e from/to":    {Name: "x", Applications: []string{"*"}, Environments: []string{"*"}, Cron: "0 18 * * FRI", Duration: time.Hour, From: now},
	} {
		if _, err := New(nil, []Window{w}); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
	if _, err := New([]Rule{{Environments: []string{"prod"}}}, nil); err == nil {
		t.Error("regola senza applications: atteso errore")
	}
}

func TestFreezePerRegion(t *testing.T) {
	now := time.Date(2026, 12, 24, 12, 0, 0, 0, time.UTC)
	p, err := New(nil, []Window{{Name: "natale-us", Applications: []string{"*"}, Environments: []string{"prod"}, Regions: []string{"us-*"},
		From: now.Add(-time.Hour), To: now.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Frozen("app", "prod", now) != nil {
		t.Error("un freeze di region non blocca l'accodamento")
	}
	if p.FrozenIn("app", "prod", "eu-1", now) != nil {
		t.Error("freeze di us applicato a eu-1")
	}
	if w := p.FrozenIn("app", "prod", "us-1", now); w == nil || w.Name != "natale-us" {
		t.Errorf("freeze di us-1 non rilevato: %+v", w)
	}
}

func TestApprovalsWhen(t *testing.T) {
	rule := func(when string) []Rule {
		return []Rule{{Applications: []string{"*"}, Environments: []string{"*"}, Approvals: Approvals{Required: 1, When: when}}}
	}
	for _, w := range []string{"", ApprovalsBefore, ApprovalsRollout} {
		if _, err := New(rule(w), nil); err != nil {
			t.Errorf("when %q: %v", w, err)
		}
	}
	if _, err := New(rule("after"), nil); err == nil {
		t.Error("when non valido accettato")
	}
	if (&Approvals{When: ApprovalsRollout}).InRollout() != true || (*Approvals)(nil).InRollout() {
		t.Error("InRollout")
	}
}
