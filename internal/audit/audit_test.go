package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultiFileELog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	f, err := NewFileRecorder(path, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := Multi{LogRecorder{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, f}
	ctx := context.Background()
	for _, e := range []Entry{
		{Actor: "oidc:alice", Action: "deployment.submit", Outcome: OutcomeAllowed, Application: "myapp", Environment: "prod", Deployment: "d1"},
		{Actor: "token:ci", Action: "deployment.cancel", Outcome: OutcomeDenied, Deployment: "d1", Reason: "permesso negato"},
	} {
		if err := m.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("attese 2 righe, trovate %d", len(lines))
	}
	var e Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Actor != "token:ci" || e.Outcome != OutcomeDenied || e.Time.IsZero() {
		t.Errorf("voce inattesa: %+v", e)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "actor=oidc:alice") {
		t.Errorf("log di audit inatteso:\n%s", logs.String())
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("permessi del file di audit %v, attesi 0600", st.Mode().Perm())
	}
}

func record(t *testing.T, r Recorder, n int, actor string) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := Entry{Time: time.Now().UTC(), Actor: actor, Action: "deployment.submit", Outcome: OutcomeAllowed, Deployment: fmt.Sprintf("d%d", i), Reason: "<&>"}
		if err := r.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatenaEManomissioni(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	f, err := NewFileRecorder(path, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	record(t, f, 3, "oidc:alice")
	_ = f.Close()
	// On reopening, the chain continues from the last entry.
	f, err = NewFileRecorder(path, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	record(t, f, 2, "oidc:bob")
	_ = f.Close()

	rep, err := Verify([]string{path}, nil)
	if err != nil || rep.Entries != 5 || rep.Anchor != "" || rep.Last == "" {
		t.Fatalf("verifica inattesa: %+v %v", rep, err)
	}

	data, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(data), "\n")
	for name, tampered := range map[string]string{
		"voce modificata": strings.Join(lines[:1], "") + strings.Replace(lines[1], "oidc:alice", "oidc:mallory", 1) + strings.Join(lines[2:], ""),
		"voce rimossa":    strings.Join(lines[:2], "") + strings.Join(lines[3:], ""),
		"voci scambiate":  lines[1] + lines[0] + strings.Join(lines[2:], ""),
	} {
		p := filepath.Join(t.TempDir(), "audit.jsonl")
		_ = os.WriteFile(p, []byte(tampered), 0o600)
		if _, err := Verify([]string{p}, nil); err == nil {
			t.Errorf("%s: manomissione non rilevata", name)
		}
	}
}

func TestRotazioneEChiave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	key := []byte("chiave-segreta-di-test")
	f, err := NewFileRecorder(path, FileOptions{MaxSize: 600, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	record(t, f, 10, "token:ci")
	_ = f.Close()
	files, err := Files(path)
	if err != nil || len(files) < 3 || files[len(files)-1] != path {
		t.Fatalf("file ruotati inattesi: %v %v", files, err)
	}
	if rep, err := Verify(files, key); err != nil || rep.Entries != 10 {
		t.Fatalf("la catena deve proseguire tra i file ruotati: %+v %v", rep, err)
	}
	// Without the key (or with another one) the chain cannot be verified, so whoever edits the
	// file cannot recompute it either.
	if _, err := Verify(files, nil); err == nil {
		t.Error("verifica senza chiave: atteso errore")
	}
	// A missing rotated file breaks the chain.
	if _, err := Verify(append(files[:1:1], files[2:]...), key); err == nil {
		t.Error("file mancante non rilevato")
	}
}
