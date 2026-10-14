package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/testdb"
)

type staticSecrets map[string]string

func (s staticSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := s[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("segreto %s assente", ref)
}

// shellDialer esegue i comandi degli step personalizzati con la shell locale.
type shellDialer struct {
	mu    sync.Mutex
	hosts []string
}

func (d *shellDialer) Dial(_ context.Context, t descriptor.Target, _ string, _ customstep.SSHAuth) (customstep.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hosts = append(d.hosts, t.Host)
	return shellConn{}, nil
}

type shellConn struct{}

func (shellConn) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

func (shellConn) Close() error { return nil }

func TestStepPersonalizzati(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho \"migrazione di $1 a $MANENT_VERSION\"\necho applied=7 >> \"$MANENT_OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/purge/eu" || r.Header.Get(customstep.HeaderIdempotencyKey) == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"id": "p-9"}`)
	}))
	t.Cleanup(srv.Close)

	migrate := customspec.Spec{
		Type:    customspec.TypeCommand,
		Params:  map[string]customspec.Param{"database": {Pattern: "[a-z]+", Required: true}},
		Outputs: map[string]customspec.Output{"applied": {Pattern: "[0-9]+"}},
		SSH:     &customspec.SSH{KeySecret: "ssh-key"},
		Run:     &customspec.Action{RunOnce: true, Command: []string{tool, "{{ .Params.database }}"}},
	}
	purge := customspec.Spec{
		Type:    customspec.TypeHTTP,
		Params:  map[string]customspec.Param{"zone": {Pattern: "[a-z]+", Default: "eu"}},
		Outputs: map[string]customspec.Output{"purge_id": {Pattern: "p-[0-9]+", Path: "$.id"}},
		Run:     &customspec.Action{URL: srv.URL + "/purge/{{ .Params.zone }}"},
	}
	keys := customspec.Keys{[]byte(strings.Repeat("k", 32))}
	steps := []config.Step{
		{Name: "artifacts", Mode: config.ModeMandatory},
		{Name: "db-migrate", Mode: config.ModeOptional, Enabled: true, Spec: migrate},
		{Name: "cdn-purge", Mode: config.ModeOptional, Enabled: true, Spec: purge},
	}
	for i := range steps {
		steps[i].ApplyDefaults()
		if err := steps[i].Validate(steps[i].Name, true); err != nil {
			t.Fatal(err)
		}
	}
	reg, _ := testRegistry(t, artifactStep{})
	planner := pipeline.ConfigPlanner{Steps: steps, Registry: reg, Keys: keys}
	dialer := &shellDialer{}
	notifier := &recordNotifier{}
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Notifier = notifier
	opts.Custom = &customstep.Builder{
		Secrets: staticSecrets{"env:SSH_KEY": "key"}, Keys: keys, Dialer: dialer, HTTP: customstep.NewHTTPClient(),
		AllowHTTP: true, SecretsDir: filepath.Join(dir, "shm"), Logger: discard,
	}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Logger: discard})

	d := appDescriptor("prod", "1.4.0")
	d.Deploy.Targets = []descriptor.Target{{Host: "db1", User: "deploy"}, {Host: "db2", User: "deploy"}}
	d.Secrets = []descriptor.SecretRef{{Name: "ssh-key", Ref: "env:SSH_KEY"}}
	d.StepParams = map[string]map[string]string{"db-migrate": {"database": "shop"}}
	d.Callbacks = []descriptor.Callback{{URL: "https://hooks.example.com/manent"}}

	// Client e API vedono tipo e hash della specifica, mai comandi e URL.
	v, err := e.Validate(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, ps := range v.Plan[1:] {
		if ps.Spec != "" || ps.SpecMAC != "" || ps.SpecHash == "" || ps.Type == "" {
			t.Errorf("piano esposto: %+v", ps)
		}
	}
	if plan, _ := e.Plan(alice, d); plan[1].Spec != "" {
		t.Error("Plan espone la specifica")
	}

	sub, err := e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Wait(alice, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	outs := map[string]pipeline.Output{}
	for _, s := range res.Steps {
		outs[s.Name] = s.Output
	}
	if outs["db-migrate"]["applied"] != "7" || outs["cdn-purge"]["purge_id"] != "p-9" {
		t.Errorf("output degli step: %v", outs)
	}
	if strings.Join(dialer.hosts, ",") != "db1" {
		t.Errorf("run_once: %v", dialer.hosts)
	}
	notifier.mu.Lock()
	for _, ev := range notifier.events {
		if (ev.Step == "db-migrate" || ev.Step == "cdn-purge") && len(ev.Output) > 0 {
			t.Errorf("output dello step %s inviato ai callback: %v", ev.Step, ev.Output)
		}
	}
	notifier.mu.Unlock()

	// Parametri obbligatori e segreti mancanti sono rifiutati all'accodamento.
	bad := appDescriptor("prod", "1.4.1")
	bad.Deploy.Targets = d.Deploy.Targets
	if _, err := e.Submit(alice, bad); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), `parametro "database" obbligatorio`) || !strings.Contains(err.Error(), `segreto "ssh-key"`) {
		t.Errorf("accodamento senza parametri e segreti: %v", err)
	}
	bad.StepParams = map[string]map[string]string{"db-migrate": {"database": "Shop; drop"}}
	if _, err := e.Submit(alice, bad); err == nil {
		t.Error("valore non conforme accettato")
	}

	// Una specifica revocata non viene più eseguita, anche se già fotografata nel piano.
	opts.Custom.(*customstep.Builder).Revoked = []string{customspec.Hash(mustPlan(t, planner, d)[1].Spec)}
	d2 := *d
	d2.Artifact.Version = "1.4.2"
	sub, err = e.Submit(alice, &d2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err == nil || !strings.Contains(err.Error(), "revocata") {
		t.Errorf("specifica revocata eseguita: %v", err)
	}
}

func mustPlan(t *testing.T, p pipeline.Planner, d *descriptor.Descriptor) pipeline.Plan {
	t.Helper()
	plan, err := p.Plan(d)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestStepPersonalizzatoPerLotto(t *testing.T) {
	smoke := customspec.Spec{
		Type: customspec.TypeCommand,
		SSH:  &customspec.SSH{User: "smoke", KeySecret: "ssh-key"},
		Run:  &customspec.Action{Command: []string{"/bin/sh", "-c", "test \"$MANENT_TARGET\" != ''"}},
	}
	l := &waveLog{}
	sl := slog.New(deploylog.NewHandler(slog.NewTextHandler(io.Discard, nil)))
	reg, err := pipeline.NewRegistry(waveStep{name: "deploy", worker: "w1", log: l, slog: sl}, waveStep{name: "activation", worker: "w1", log: l, slog: sl})
	if err != nil {
		t.Fatal(err)
	}
	steps := []config.Step{
		{Name: "deploy", Mode: config.ModeMandatory},
		{Name: "smoke", Mode: config.ModeMandatory, Spec: smoke},
		{Name: "activation", Mode: config.ModeMandatory},
	}
	steps[1].ApplyDefaults()
	ch := &fakeChecker{red: map[string]bool{}}
	dialer := &shellDialer{}
	opts := workerOpts(testdb.URL(t), "w1", reg)
	opts.Health = ch
	opts.Custom = &customstep.Builder{Secrets: staticSecrets{"env:K": "k"}, Dialer: dialer, SecretsDir: t.TempDir(), Logger: discard}
	w := startWorker(t, opts)
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: pipeline.ConfigPlanner{Steps: steps, Registry: reg}, Logger: discard, Health: ch})

	d := waveDescriptor("ondate-custom")
	d.Secrets = []descriptor.SecretRef{{Name: "ssh-key", Ref: "env:K"}}
	sub, err := e.Submit(alice, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(alice, sub.ID); err != nil {
		t.Fatal(err)
	}
	// Tra deploy e activation lo step gira per ogni lotto, sui soli target del lotto.
	if got := strings.Join(dialer.hosts, ","); got != "eu-1a,eu-1b,eu-2a,us-1a,eu-2b,us-1b" {
		t.Errorf("target dello step personalizzato: %s", got)
	}
	steps2, err := e.Steps(alice, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range steps2 {
		found = found || s.Name == "smoke@canary#0"
	}
	if !found {
		t.Errorf("step DBOS del lotto non trovato: %+v", steps2)
	}
}
