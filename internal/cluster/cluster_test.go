package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/testdb"
)

func openTest(t *testing.T) *SQL {
	t.Helper()
	r, err := Open(context.Background(), testdb.URL(t))
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
		ok, err := r.Register(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !register(Executor{ID: "w1", Version: "1", Build: "dev", Hostname: "h1", StartedAt: start}) {
		t.Fatal("prima registrazione rifiutata")
	}
	// Executor attivo: non rivendicabile.
	if ok, err := r.Claim(ctx, "w1", "w2", now.Add(-time.Minute), false); err != nil || ok {
		t.Fatalf("claim di un executor attivo: ok=%v err=%v", ok, err)
	}
	now = now.Add(10 * time.Minute)
	if ok, err := r.Claim(ctx, "w1", "w2", now.Add(-time.Minute), false); err != nil || !ok {
		t.Fatalf("claim di un executor perso: ok=%v err=%v", ok, err)
	}
	// Già rivendicato: un secondo worker non lo riprende.
	if ok, _ := r.Claim(ctx, "w1", "w3", now.Add(-time.Minute), false); ok {
		t.Fatal("doppia rivendicazione")
	}
	es, err := r.List(ctx)
	if err != nil || len(es) != 1 || es[0].RecoveredBy != "w2" || es[0].Hostname != "h1" {
		t.Fatalf("registro inatteso: %+v %v", es, err)
	}
	// Il processo che torna a rispondere scopre di essere stato recuperato.
	if by, err := r.Heartbeat(ctx, "w1", start); err != nil || by != "w2" {
		t.Fatalf("heartbeat: by=%q err=%v", by, err)
	}
	// Un nuovo avvio attende la fine della recovery in corso...
	if register(Executor{ID: "w1", Version: "2", StartedAt: now}) {
		t.Fatal("registrazione accettata durante la recovery")
	}
	if err := r.ClaimDone(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	// ...poi azzera la rivendicazione.
	if !register(Executor{ID: "w1", Version: "2", StartedAt: now}) {
		t.Fatal("registrazione rifiutata dopo la recovery")
	}
	es, _ = r.List(ctx)
	if es[0].RecoveredBy != "" || es[0].Version != "2" || es[0].Stale(now, time.Minute) {
		t.Fatalf("nuovo avvio non registrato: %+v", es[0])
	}
	// Il vecchio processo non aggiorna più la riga.
	if by, _ := r.Heartbeat(ctx, "w1", start); by != "" {
		t.Fatalf("heartbeat di un avvio superato: %q", by)
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
	// Una rivendicazione abbandonata (chi recuperava è caduto) si può riprendere dopo ClaimTimeout.
	if ok, _ := r.Claim(ctx, "w1", "w3", now, false); ok {
		t.Fatal("rivendicazione in corso sottratta")
	}
	now = now.Add(ClaimTimeout + time.Second)
	if ok, _ := r.Claim(ctx, "w1", "w3", now, false); !ok {
		t.Fatal("rivendicazione abbandonata non ripresa")
	}
	// Executor sconosciuto: solo con force.
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
