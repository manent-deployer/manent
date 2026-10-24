package extplugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// fakeBin is the testdata/fakeplugin binary, built once in TestMain.
var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "extplugin")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakeplugin")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeplugin").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build del plugin di test: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer collects logs safely across goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// mapSecrets resolves secrets from a map.
type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", errors.New("segreto " + name + " non trovato")
	}
	return v, nil
}

func checksum(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newProcess activates the test plugin with the given configuration.
func newProcess(t *testing.T, ext map[string]any, extra map[string]any) (*Process, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	cfg := map[string]any{Key: ext}
	for k, v := range extra {
		cfg[k] = v
	}
	d, err := NewFactory("fake-ext")(cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Process)
	t.Cleanup(func() { _ = p.Close() })
	return p, logs
}

func request(params map[string]string) *plugin.Request {
	return &plugin.Request{
		DeploymentID: "d1",
		Artifact:     plugin.Artifact{Type: "application", LocalPath: "/tmp/app.tgz"},
		Targets:      []plugin.Target{{Host: "h1"}},
		Params:       params,
		Secrets:      mapSecrets{"tok": "s3cr3t"},
	}
}

func TestDeploy(t *testing.T) {
	t.Setenv("LEAK_ME", "valore-del-worker")
	p, logs := newProcess(t, map[string]any{"command": fakeBin, "sha256": checksum(t, fakeBin), "env": map[string]any{"FOO": "bar"}}, nil)

	if p.Name() != "fake-ext" || !p.Supports("application") || p.Supports("container") {
		t.Fatal("nome o tipi supportati inattesi")
	}
	if err := p.Validate(request(nil)); err == nil || !strings.Contains(err.Error(), "dest parameter is required") {
		t.Fatalf("errore di validazione non propagato: %v", err)
	}
	res, err := p.Deploy(context.Background(), request(map[string]string{"dest": "/opt", "secret": "tok"}))
	if err != nil {
		t.Fatal(err)
	}
	det := res.Targets[0].Details
	// The secret arrives through the resolver; the worker's variables are not inherited.
	if det["secret_len"] != "6" || det["leak"] != "" || det["foo"] != "bar" || res.Targets[0].Status != plugin.StatusDeployed {
		t.Errorf("risultato inatteso: %+v", res.Targets[0])
	}
	// The outputs cross the protocol (exports of the deploy).
	if res.Outputs["vms"] != `["10.0.0.1"]` {
		t.Errorf("output del plugin: %+v", res.Outputs)
	}
	if _, err := p.Deploy(context.Background(), request(map[string]string{"dest": "/opt", "secret": "manca"})); err == nil {
		t.Error("atteso errore per segreto mancante")
	}
	// Plugin logs reach manent's logger, asynchronously with respect to the response.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `msg="fake deploy done"`) {
		if time.Now().After(deadline) {
			t.Fatalf("log del plugin non inoltrato:\n%s", logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "deployment=d1") {
		t.Errorf("attributi del log del plugin persi:\n%s", logs)
	}
}

func TestDryRun(t *testing.T) {
	p, _ := newProcess(t, map[string]any{"command": fakeBin}, nil)
	if _, err := p.DryRun(context.Background(), request(nil)); !errors.Is(err, plugin.ErrDryRunUnsupported) {
		t.Fatalf("atteso ErrDryRunUnsupported, ricevuto %v", err)
	}
	p, _ = newProcess(t, map[string]any{"command": fakeBin}, map[string]any{"dry_run": true})
	res, err := p.DryRun(context.Background(), request(nil))
	if err != nil || res.Targets[0].Status != plugin.StatusReady {
		t.Fatalf("dry-run inatteso: %+v %v", res, err)
	}
}

func TestRollback(t *testing.T) {
	ctx := context.Background()
	// Plugin without rollback: the declared capabilities exclude it.
	p, _ := newProcess(t, map[string]any{"command": fakeBin}, nil)
	if ok, err := plugin.CanRollback(ctx, p); err != nil || ok {
		t.Fatalf("rollback non atteso: %v %v", ok, err)
	}
	if _, err := p.Rollback(ctx, request(nil), plugin.Release{Version: "1.0.0"}); !errors.Is(err, plugin.ErrRollbackUnsupported) {
		t.Fatalf("atteso ErrRollbackUnsupported, ricevuto %v", err)
	}
	if _, err := (plugin.PluginLocator{}).Current(ctx, p, request(nil)); err == nil {
		t.Fatal("attesa release non leggibile")
	}

	p, _ = newProcess(t, map[string]any{"command": fakeBin}, map[string]any{"rollback": true})
	if ok, err := plugin.CanRollback(ctx, p); err != nil || !ok {
		t.Fatalf("rollback atteso: %v %v", ok, err)
	}
	rel, err := (plugin.PluginLocator{}).Current(ctx, p, request(nil))
	if err != nil || rel == nil || rel.Version != "1.0.0" || rel.Details["source"] != "fake" {
		t.Fatalf("release inattesa: %+v %v", rel, err)
	}
	if rel, err := p.CurrentRelease(ctx, request(map[string]string{"first": "true"})); err != nil || rel != nil {
		t.Fatalf("attesa nessuna release: %+v %v", rel, err)
	}
	res, err := p.Rollback(ctx, request(map[string]string{"secret": "tok"}), *rel)
	if err != nil {
		t.Fatal(err)
	}
	if tr := res.Targets[0]; tr.Status != plugin.StatusRolledBack || tr.Details["release"] != "1.0.0" || tr.Details["secret_len"] != "6" {
		t.Errorf("rollback inatteso: %+v", tr)
	}
}

func TestRiavvioDopoCrash(t *testing.T) {
	p, _ := newProcess(t, map[string]any{"command": fakeBin}, nil)
	ok := map[string]string{"dest": "/opt", "secret": "tok"}
	first, err := p.Deploy(context.Background(), request(ok))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(context.Background(), request(map[string]string{"dest": "/opt", "crash": "true"})); err == nil {
		t.Fatal("atteso errore per crash del plugin")
	}
	second, err := p.Deploy(context.Background(), request(ok))
	if err != nil {
		t.Fatalf("il plugin doveva essere riavviato: %v", err)
	}
	if first.Targets[0].Details["pid"] == second.Targets[0].Details["pid"] {
		t.Error("atteso un nuovo processo dopo il crash")
	}
}

func TestClose(t *testing.T) {
	p, _ := newProcess(t, map[string]any{"command": fakeBin}, nil)
	if !p.Supports("application") {
		t.Fatal("plugin non avviato")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(context.Background(), request(nil)); err == nil {
		t.Error("atteso errore dopo Close")
	}
}

func TestAttivazioneErrori(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	tests := map[string]map[string]any{
		"senza command":     {Key: map[string]any{}},
		"binario assente":   {Key: map[string]any{"command": "/non/esiste"}},
		"sha256 diverso":    {Key: map[string]any{"command": fakeBin, "sha256": strings.Repeat("0", 64)}},
		"campo sconosciuto": {Key: map[string]any{"command": fakeBin, "comando": "x"}},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFactory("x")(cfg, log); err == nil {
				t.Fatal("atteso errore")
			}
		})
	}
	// A configuration rejected by the plugin shows up at the first start, not at activation.
	d, err := NewFactory("x")(map[string]any{Key: map[string]any{"command": fakeBin}, "fail": true}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer d.(*Process).Close()
	if err := d.Validate(request(nil)); err == nil || !strings.Contains(err.Error(), "configuration rejected") {
		t.Errorf("errore di configurazione non propagato: %v", err)
	}
}

func TestRilascioPerPercentuale(t *testing.T) {
	ctx := context.Background()
	// Plugin without slots: the declared capabilities exclude it and the calls are unsupported.
	p, _ := newProcess(t, map[string]any{"command": fakeBin}, nil)
	if s, err := plugin.SupportsTraffic(ctx, p); err != nil || s.Slots || s.Router {
		t.Fatalf("supporto inatteso: %+v %v", s, err)
	}
	if _, err := p.PrepareSlots(ctx, request(nil)); !errors.Is(err, plugin.ErrTrafficUnsupported) {
		t.Fatalf("atteso ErrTrafficUnsupported, ricevuto %v", err)
	}

	p, _ = newProcess(t, map[string]any{"command": fakeBin}, map[string]any{"slots": true})
	if s, err := plugin.SupportsTraffic(ctx, p); err != nil || !s.Slots || !s.Router || s.Testers || s.Scale {
		t.Fatalf("supporto: %+v %v", s, err)
	}
	req := request(map[string]string{"secret": "tok"})
	state, err := p.PrepareSlots(ctx, req)
	if err != nil || state["stable"] != "a" {
		t.Fatalf("stato: %v %v", state, err)
	}
	req.Traffic = &plugin.TrafficRequest{State: state, TesterHeader: "x-canary: 1"}
	res, err := p.DeployCandidate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// Rollout state and secrets reach the plugin process.
	if d := res.Targets[0].Details; d["stable"] != "a" || d["header"] != "x-canary: 1" || d["secret_len"] != "6" {
		t.Errorf("candidata: %v", d)
	}
	if err := p.SetCandidateWeight(ctx, req, 25); err != nil {
		t.Fatal(err)
	}
	if w, err := p.CandidateWeight(ctx, req); err != nil || w != 25 {
		t.Errorf("peso: %d %v", w, err)
	}
	for _, fn := range []func(context.Context, *plugin.Request) error{p.PromoteCandidate, p.RevertCandidate, p.RetirePrevious} {
		if err := fn(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.RouteTesters(ctx, req); !errors.Is(err, plugin.ErrTrafficUnsupported) {
		t.Errorf("tester non supportati: %v", err)
	}
}
