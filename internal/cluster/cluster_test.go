package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/testdb"
)

func openTest(t *testing.T) *SQL {
	t.Helper()
	return openURL(t, testdb.URL(t))
}

func openURL(t *testing.T, url string) *SQL {
	t.Helper()
	r, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRegistrazioneEClaim(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	start := now.Add(-time.Hour)
	register := func(e Executor) bool {
		t.Helper()
		ok, err := r.Register(ctx, e, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !register(Executor{ID: "w1", Version: "1", Build: "dev", Hostname: "h1", StartedAt: start}) {
		t.Fatal("prima registrazione rifiutata")
	}
	// Live executor: cannot be claimed.
	if ok, err := r.Claim(ctx, "w1", "w2", now.Add(-time.Minute), false); err != nil || ok {
		t.Fatalf("claim di un executor attivo: ok=%v err=%v", ok, err)
	}
	now = now.Add(10 * time.Minute)
	if ok, err := r.Claim(ctx, "w1", "w2", now.Add(-time.Minute), false); err != nil || !ok {
		t.Fatalf("claim di un executor perso: ok=%v err=%v", ok, err)
	}
	// Already claimed: a second worker does not resume it.
	if ok, _ := r.Claim(ctx, "w1", "w3", now.Add(-time.Minute), false); ok {
		t.Fatal("doppia rivendicazione")
	}
	es, err := r.List(ctx)
	if err != nil || len(es) != 1 || es[0].RecoveredBy != "w2" || es[0].Hostname != "h1" {
		t.Fatalf("registro inatteso: %+v %v", es, err)
	}
	// The process that answers again finds out it has been recovered.
	if by, err := r.Heartbeat(ctx, "w1", start); err != nil || by != "w2" {
		t.Fatalf("heartbeat: by=%q err=%v", by, err)
	}
	// A new startup waits for the running recovery to end...
	if register(Executor{ID: "w1", Version: "2", StartedAt: now}) {
		t.Fatal("registrazione accettata durante la recovery")
	}
	if err := r.ClaimDone(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	// ...then clears the claim.
	if !register(Executor{ID: "w1", Version: "2", StartedAt: now}) {
		t.Fatal("registrazione rifiutata dopo la recovery")
	}
	es, _ = r.List(ctx)
	if es[0].RecoveredBy != "" || es[0].Version != "2" || es[0].Stale(now, time.Minute) {
		t.Fatalf("nuovo avvio non registrato: %+v", es[0])
	}
	// The old process no longer updates the row and finds out it has been replaced.
	if by, err := r.Heartbeat(ctx, "w1", start); by != "" || !errors.Is(err, ErrReplaced) {
		t.Fatalf("heartbeat di un avvio superato: %q %v", by, err)
	}
	if err := r.Stopped(ctx, "w1", now); err != nil {
		t.Fatal(err)
	}
	es, _ = r.List(ctx)
	if !es[0].Stale(now, time.Hour) {
		t.Fatal("un executor arrestato va considerato perso")
	}
	if ok, _ := r.Claim(ctx, "w1", "w2", now.Add(-time.Hour), false); !ok {
		t.Fatal("un executor arrestato è rivendicabile subito")
	}
	// An abandoned claim (the recovering worker crashed) can be taken over after ClaimTimeout.
	if ok, _ := r.Claim(ctx, "w1", "w3", now, false); ok {
		t.Fatal("rivendicazione in corso sottratta")
	}
	now = now.Add(ClaimTimeout + time.Second)
	if ok, _ := r.Claim(ctx, "w1", "w3", now, false); !ok {
		t.Fatal("rivendicazione abbandonata non ripresa")
	}
	// Unknown executor: only with force.
	if ok, _ := r.Claim(ctx, "vecchio", "admin", now, false); ok {
		t.Fatal("executor sconosciuto rivendicato senza force")
	}
	if ok, _ := r.Claim(ctx, "vecchio", "admin", now, true); !ok {
		t.Fatal("executor sconosciuto non rivendicato con force")
	}
}

func TestLease(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	now := time.Now()
	r.now = func() time.Time { return now }
	if ok, err := r.Lease(ctx, "retention", "w1", time.Minute); err != nil || !ok {
		t.Fatalf("lease libero: ok=%v err=%v", ok, err)
	}
	if ok, _ := r.Lease(ctx, "retention", "w2", time.Minute); ok {
		t.Fatal("lease occupato acquisito")
	}
	if ok, _ := r.Lease(ctx, "retention", "w1", time.Minute); !ok {
		t.Fatal("rinnovo del proprio lease fallito")
	}
	now = now.Add(2 * time.Minute)
	if ok, _ := r.Lease(ctx, "retention", "w2", time.Minute); !ok {
		t.Fatal("lease scaduto non acquisito")
	}
}

func TestDataSource(t *testing.T) {
	for in, want := range map[string]string{
		"sqlite:manent.db":         "manent.db?_pragma=busy_timeout(10000)",
		"sqlite:///tmp/x.db":       "/tmp/x.db?_pragma=busy_timeout(10000)",
		"sqlite:/tmp/x.db?mode=rw": "/tmp/x.db?mode=rw&_pragma=busy_timeout(10000)",
	} {
		if _, dsn, pg, err := dataSource(in); err != nil || pg || dsn != want {
			t.Errorf("%s: dsn=%q pg=%v err=%v", in, dsn, pg, err)
		}
	}
	if _, _, pg, err := dataSource("postgres://u:p@h/db"); err != nil || !pg {
		t.Error("postgres non riconosciuto")
	}
	if _, _, _, err := dataSource("sqlite::memory:"); err == nil {
		t.Error("SQLite in memoria accettato")
	}
}

func TestRegionERegistroPrecedente(t *testing.T) {
	ctx := context.Background()
	url := testdb.URL(t)
	r, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Registry created by an earlier version, without the region column.
	if _, err := r.db.ExecContext(ctx, `ALTER TABLE `+r.executors+` DROP COLUMN region`); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	for range 2 { // the migration can be repeated
		if r, err = Open(ctx, url); err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
	}
	r = openURL(t, url)
	if _, err := r.Register(ctx, Executor{ID: "w1", Version: "1", Region: "eu", StartedAt: time.Now()}, time.Minute); err != nil {
		t.Fatal(err)
	}
	es, err := r.List(ctx)
	if err != nil || len(es) != 1 || es[0].Region != "eu" {
		t.Fatalf("region non registrata: %+v %v", es, err)
	}
}

// Two processes with the same executor_id: the second is rejected while the first sends
// heartbeats; after an orderly shutdown or no heartbeat for staleAfter the registration succeeds.
func TestExecutorIDInUso(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	first := Executor{ID: "host1", Version: "1", Hostname: "host1", StartedAt: now}
	if ok, err := r.Register(ctx, first, time.Minute); err != nil || !ok {
		t.Fatalf("prima istanza: %v %v", ok, err)
	}
	second := Executor{ID: "host1", Version: "1", Hostname: "host1", StartedAt: now.Add(time.Second)}
	now = now.Add(30 * time.Second)
	_, err := r.Register(ctx, second, time.Minute)
	var inUse *InUseError
	if !errors.As(err, &inUse) || inUse.Hostname != "host1" {
		t.Fatalf("seconda istanza con lo stesso ID accettata: %v", err)
	}
	// The first instance stays registered and its shutdown does not affect others.
	if _, err := r.Heartbeat(ctx, "host1", first.StartedAt); err != nil {
		t.Fatalf("heartbeat della prima istanza: %v", err)
	}
	// Restart of the same instance (same startup): allowed.
	if ok, err := r.Register(ctx, first, time.Minute); err != nil || !ok {
		t.Fatalf("ripetizione della registrazione: %v %v", ok, err)
	}
	// The first crashes: with no heartbeat for staleAfter the second takes over.
	now = now.Add(2 * time.Minute)
	if ok, err := r.Register(ctx, second, time.Minute); err != nil || !ok {
		t.Fatalf("subentro dopo il crash: %v %v", ok, err)
	}
	// Orderly shutdown: the next startup takes over at once.
	if err := r.Stopped(ctx, "host1", second.StartedAt); err != nil {
		t.Fatal(err)
	}
	third := Executor{ID: "host1", Version: "1", Hostname: "host1", StartedAt: now.Add(time.Second)}
	if ok, err := r.Register(ctx, third, time.Minute); err != nil || !ok {
		t.Fatalf("avvio dopo un arresto ordinato: %v %v", ok, err)
	}
}

func TestStatoDeiTarget(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for _, rec := range []TargetsRecord{
		{Application: "shop", Environment: "prod", Scope: "eu-1", CheckedAt: at, Data: `{"v":1}`},
		{Application: "shop", Environment: "prod", Scope: "", CheckedAt: at, Data: `{"v":2}`},
		// Same scope again: the record is replaced.
		{Application: "shop", Environment: "prod", Scope: "eu-1", CheckedAt: at.Add(time.Minute), Data: `{"v":3}`},
	} {
		if err := r.SaveTargets(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Scope != "" || got[1].Data != `{"v":3}` || !got[1].CheckedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("record: %+v", got)
	}
}

func TestRevisioniDellaPolicy(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	if rev, err := r.ActivePolicy(ctx); err != nil || rev != 0 {
		t.Fatalf("nessuna revisione attiva attesa: %d %v", rev, err)
	}
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	r1, err := r.CreatePolicy(ctx, PolicyRecord{SHA256: "a", Author: "oidc:alice", CreatedAt: now, Tests: "[]", Files: `{"p.rego":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := r.CreatePolicy(ctx, PolicyRecord{SHA256: "b", Author: "oidc:alice", CreatedAt: now, Tests: "[]", Files: `{}`})
	if err != nil || r2 <= r1 {
		t.Fatalf("revisioni %d %d: %v", r1, r2, err)
	}
	if err := r.ActivatePolicy(ctx, r1, 0, "oidc:bob"); err != nil {
		t.Fatal(err)
	}
	// The expected active revision is 0, but r1 is active now: conflict.
	if err := r.ActivatePolicy(ctx, r2, 0, "oidc:bob"); !errors.Is(err, ErrPolicyConflict) {
		t.Fatalf("conflitto atteso: %v", err)
	}
	if err := r.ActivatePolicy(ctx, r2, r1, ""); err != nil {
		t.Fatal(err)
	}
	ps, err := r.Policies(ctx)
	if err != nil || len(ps) != 2 || ps[0].Revision != r2 || ps[0].Status != policyActive || ps[1].Status != policySuperseded || ps[1].ReviewedBy != "oidc:bob" {
		t.Fatalf("revisioni inattese: %+v %v", ps, err)
	}
	p, err := r.Policy(ctx, r1)
	if err != nil || p == nil || p.Files != `{"p.rego":"x"}` {
		t.Fatalf("revisione %d: %+v %v", r1, p, err)
	}
	if p, err := r.Policy(ctx, 99); err != nil || p != nil {
		t.Fatalf("revisione inesistente: %+v %v", p, err)
	}

	if err := r.CreateFreeze(ctx, FreezeRecord{Name: "incidente", Data: "{}", CreatedBy: "oidc:carol", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateFreeze(ctx, FreezeRecord{Name: "incidente", Data: "{}", CreatedBy: "oidc:carol", CreatedAt: now}); err == nil {
		t.Fatal("nome duplicato accettato")
	}
	fs, err := r.ListFreezes(ctx)
	if err != nil || len(fs) != 1 || fs[0].CreatedBy != "oidc:carol" {
		t.Fatalf("freeze inattesi: %+v %v", fs, err)
	}
	if ok, err := r.DeleteFreeze(ctx, "incidente"); err != nil || !ok {
		t.Fatalf("eliminazione: %v %v", ok, err)
	}
	if ok, _ := r.DeleteFreeze(ctx, "incidente"); ok {
		t.Fatal("seconda eliminazione riuscita")
	}
}

func TestNotificaDellaPolicy(t *testing.T) {
	if !testdb.Postgres() {
		t.Skip("LISTEN/NOTIFY solo su Postgres")
	}
	r := openTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan struct{}, 4)
	go func() { _ = r.ListenPolicy(ctx, func() { got <- struct{}{} }) }()
	// The first call arrives when listening starts.
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("ascolto non avviato")
	}
	if err := r.NotifyPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("notifica non ricevuta")
	}
}

func TestCodeERisorseDelWorker(t *testing.T) {
	r := openTest(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Minute)
	e := Executor{ID: "w1", Version: "1", Build: "dev", Hostname: "h1", StartedAt: start,
		Queues: []string{"deployments", "deployments-approvals"}, Resources: json.RawMessage(`{"cpus":4}`)}
	if ok, err := r.Register(ctx, e, time.Minute); err != nil || !ok {
		t.Fatalf("registrazione: ok=%v err=%v", ok, err)
	}
	if err := r.Report(ctx, "w1", start, json.RawMessage(`{"cpus":4,"goroutines":12}`)); err != nil {
		t.Fatal(err)
	}
	// A sample of another startup with the same ID is ignored.
	if err := r.Report(ctx, "w1", start.Add(time.Second), json.RawMessage(`{"cpus":1}`)); err != nil {
		t.Fatal(err)
	}
	es, err := r.List(ctx)
	if err != nil || len(es) != 1 {
		t.Fatalf("registro: %+v %v", es, err)
	}
	if got := strings.Join(es[0].Queues, ","); got != "deployments,deployments-approvals" {
		t.Errorf("code: %q", got)
	}
	if string(es[0].Resources) != `{"cpus":4,"goroutines":12}` {
		t.Errorf("risorse: %s", es[0].Resources)
	}
	// An executor registered without queues and resources reads them as empty.
	if ok, err := r.Register(ctx, Executor{ID: "w2", Version: "1", Build: "dev", Hostname: "h2", StartedAt: start}, time.Minute); err != nil || !ok {
		t.Fatal(err)
	}
	if es, _ := r.List(ctx); len(es) != 2 || es[1].Queues != nil || es[1].Resources != nil {
		t.Errorf("executor senza code e risorse: %+v", es)
	}
}
