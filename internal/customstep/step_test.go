package customstep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

type mapSecrets map[string]string

func (m mapSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("segreto %s assente", ref)
}

// LocalDialer runs the commands with the local shell: same quoting rules as a target reached
// over SSH.
type localDialer struct {
	mu    sync.Mutex
	dials []string
}

func (d *localDialer) Dial(_ context.Context, t descriptor.Target, user string, auth SSHAuth) (Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials = append(d.dials, t.Host+":"+user+":"+auth.PrivateKey)
	return localConn{}, nil
}

type localConn struct{}

func (localConn) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (localConn) Close() error { return nil }

type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Record(_ context.Context, e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

// syncBuffer collects the logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const secretValue = "s3cr3t-db-password"

// tool is the test command: it records arguments, environment, secrets and writes an output.
const tool = `#!/bin/sh
state=$1; code=$2; shift 2
for a in "$@"; do printf 'arg=[%s]\n' "$a"; done
env | grep '^MANENT_' | sort > "$state.env"
cat "$MANENT_SECRETS_DIR/db-admin" > "$state.secret" 2>/dev/null || echo none > "$state.secret"
echo "$MANENT_SECRETS_DIR" > "$state.dir"
echo "password letta: $(cat "$MANENT_SECRETS_DIR/db-admin" 2>/dev/null)"
echo "avviso su stderr" >&2
echo "applied=3" >> "$MANENT_OUTPUT"
exit "$code"
`

type fixture struct {
	b     *Builder
	dial  *localDialer
	audit *memAudit
	logs  *syncBuffer
	state string
	tool  string
	keys  customspec.Keys
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	toolPath := filepath.Join(dir, "tool.sh")
	if err := os.WriteFile(toolPath, []byte(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{dial: &localDialer{}, audit: &memAudit{}, logs: &syncBuffer{}, state: filepath.Join(dir, "state"), tool: toolPath,
		keys: customspec.Keys{[]byte(strings.Repeat("k", 32))}}
	f.b = &Builder{
		Secrets: mapSecrets{"vault:db": secretValue + "\n", "vault:ro": "readonly-pass", "vault:ssh": "PRIVATE-KEY", "vault:cdn": "cdn-token-value"},
		Keys:    f.keys, Dialer: f.dial, HTTP: NewHTTPClient(), AllowHTTP: true,
		SecretsDir: filepath.Join(dir, "shm"), Audit: f.audit,
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	return f
}

func (f *fixture) commandSpec(args ...string) customspec.Spec {
	s := customspec.Spec{
		Type:    customspec.TypeCommand,
		Secrets: []string{"db-admin"},
		Params: map[string]customspec.Param{
			"database": {Pattern: "[a-z]+", Required: true},
			"code":     {Pattern: "[0-9]+", Default: "0"},
			"label":    {Pattern: "[^\\n]*", Default: "x"},
		},
		Outputs: map[string]customspec.Output{"applied": {Pattern: "[0-9]+"}},
		SSH:     &customspec.SSH{KeySecret: "ssh-key"},
		Run: &customspec.Action{RunOnce: true, Command: append([]string{f.tool, f.state, "{{ .Params.code }}",
			"--to={{ .Version }}", "{{ .Params.database }}", "{{ .Params.label }}"}, args...)},
		DryRun: &customspec.Action{Command: []string{f.tool, f.state, "0", "plan"}, Secrets: []string{"db-readonly"}},
	}
	s.ApplyDefaults()
	return s
}

func (f *fixture) planned(t *testing.T, name string, s customspec.Spec) pipeline.PlannedStep {
	t.Helper()
	if err := s.Validate(name, true); err != nil {
		t.Fatal(err)
	}
	doc, err := customspec.Encode(name, s)
	if err != nil {
		t.Fatal(err)
	}
	return pipeline.PlannedStep{Name: name, Type: s.Type, Spec: doc, SpecMAC: f.keys.Sign(doc), SpecHash: customspec.Hash(doc)}
}

func input(params map[string]string) pipeline.Input {
	d := &descriptor.Descriptor{
		Application: "shop", Environment: "prod",
		Artifact: descriptor.Artifact{Name: "shop", Version: "1.4.0"},
		Secrets: []descriptor.SecretRef{
			{Name: "db-admin", Ref: "vault:db"}, {Name: "db-readonly", Ref: "vault:ro"},
			{Name: "ssh-key", Ref: "vault:ssh"}, {Name: "cdn-token", Ref: "vault:cdn"},
		},
		Deploy:     descriptor.Deploy{Targets: []descriptor.Target{{Host: "db1", User: "deploy"}, {Host: "db2", User: "deploy"}}},
		StepParams: map[string]map[string]string{"db-migrate": params},
	}
	return pipeline.Input{DeploymentID: "shop-1.4.0-prod", Descriptor: d, Outputs: map[string]pipeline.Output{"artifacts": {"sha256": "abc123"}}}
}

func TestComando(t *testing.T) {
	f := newFixture(t)
	ps := f.planned(t, "db-migrate", f.commandSpec())
	label := `a b'c "$(touch /tmp/manent-pwned)" ; rm -rf /`
	out, err := f.b.Build(ps, "oidc:alice").Run(context.Background(), input(map[string]string{"database": "shop", "label": label}))
	if err != nil {
		t.Fatalf("esecuzione: %v\nlog:\n%s", err, f.logs)
	}
	if out["applied"] != "3" || len(out) != 1 {
		t.Errorf("output: %v", out)
	}
	if got := f.dial.dials; len(got) != 1 || got[0] != "db1:deploy:PRIVATE-KEY" {
		t.Errorf("connessioni (run_once): %v", got)
	}
	logs := f.logs.String()
	// Arguments arrive intact, without shell interpretation.
	for _, want := range []string{"arg=[--to=1.4.0]", "arg=[shop]", "avviso su stderr", "password letta: ***"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log senza %q:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, `c \"$(touch /tmp/manent-pwned)\" ; rm -rf /]`) {
		t.Errorf("argomento con caratteri della shell alterato:\n%s", logs)
	}
	if _, err := os.Stat("/tmp/manent-pwned"); err == nil {
		t.Error("un parametro è stato interpretato dalla shell")
	}
	if strings.Contains(logs, secretValue) {
		t.Error("il segreto compare nel log")
	}
	env, _ := os.ReadFile(f.state + ".env")
	for _, want := range []string{"MANENT_STEP=db-migrate", "MANENT_PARAM_DATABASE=shop", "MANENT_VERSION=1.4.0", "MANENT_DEPLOYMENT_ID=shop-1.4.0-prod",
		"MANENT_OUTPUT_ARTIFACTS_SHA256=abc123", "MANENT_DRY_RUN=false", "MANENT_TARGET=db1"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("ambiente senza %s:\n%s", want, env)
		}
	}
	if sec, _ := os.ReadFile(f.state + ".secret"); string(sec) != secretValue+"\n" {
		t.Errorf("segreto nel file: %q", sec)
	}
	dir, _ := os.ReadFile(f.state + ".dir")
	if _, err := os.Stat(strings.TrimSpace(string(dir))); !os.IsNotExist(err) {
		t.Errorf("directory dei segreti non rimossa: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.b.SecretsDir, "shop-1.4.0-prod")); len(entries) != 0 {
		t.Errorf("directory del deploy non vuota: %v", entries)
	}
	if n := len(f.audit.entries); n != 1 || f.audit.entries[0].Actor != "oidc:alice" || f.audit.entries[0].Spec != ps.SpecHash ||
		f.audit.entries[0].Step != "db-migrate" || f.audit.entries[0].Outcome != audit.OutcomeSucceeded {
		t.Errorf("audit: %+v", f.audit.entries)
	}
}

func TestComandoDryRun(t *testing.T) {
	f := newFixture(t)
	step := f.b.Build(f.planned(t, "db-migrate", f.commandSpec()), "oidc:alice")
	dr, ok := step.(pipeline.DryRunner)
	if !ok {
		t.Fatal("lo step con dry_run deve implementare DryRunner")
	}
	in := input(map[string]string{"database": "shop"})
	in.Descriptor.DryRun = true
	if _, err := dr.DryRun(context.Background(), in); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	// The dry-run receives only its own secrets, not run's.
	if sec, _ := os.ReadFile(f.state + ".secret"); string(sec) != "none\n" {
		t.Errorf("il dry-run ha ricevuto il segreto di run: %q", sec)
	}
	if env, _ := os.ReadFile(f.state + ".env"); !strings.Contains(string(env), "MANENT_DRY_RUN=true") {
		t.Errorf("ambiente del dry-run:\n%s", env)
	}
	// During a freeze commands do not run even in dry-run, unless overridden.
	f.b.Frozen = func(string, string, time.Time) bool { return true }
	before := len(f.dial.dials)
	out, err := dr.DryRun(context.Background(), in)
	if err != nil || out[OutDryRun] != "skipped" || out[OutReason] != "freeze" || len(f.dial.dials) != before {
		t.Errorf("dry-run durante il freeze: %v, %v", out, err)
	}
	in.Descriptor.FreezeOverride = "INC-42"
	if _, err := dr.DryRun(context.Background(), in); err != nil || len(f.dial.dials) != before+1 {
		t.Errorf("dry-run con deroga: %v", err)
	}
	s := f.commandSpec()
	s.DryRun = nil
	if _, ok := f.b.Build(f.planned(t, "db-migrate", s), "").(pipeline.DryRunner); ok {
		t.Error("uno step senza dry_run non deve implementare DryRunner")
	}
}

func TestComandoErrori(t *testing.T) {
	f := newFixture(t)
	run := func(ps pipeline.PlannedStep, params map[string]string) error {
		_, err := f.b.Build(ps, "oidc:alice").Run(context.Background(), input(params))
		return err
	}
	ps := f.planned(t, "db-migrate", f.commandSpec())
	ok := map[string]string{"database": "shop"}

	if err := run(ps, map[string]string{"database": "shop", "label": "-oProxyCommand=evil"}); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "option") {
		t.Errorf("argument injection: %v", err)
	}
	// An empty parameter takes the default; a placeholder without a value is an error, never an
	// empty string.
	s0 := f.commandSpec()
	s0.Params["label"] = customspec.Param{Pattern: "[a-z]*"}
	if err := run(f.planned(t, "db-migrate", s0), map[string]string{"database": "shop", "label": ""}); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), `"label"`) {
		t.Errorf("valore vuoto: %v", err)
	}
	if err := run(ps, map[string]string{}); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "required") {
		t.Errorf("parametro obbligatorio: %v", err)
	}
	if err := run(ps, map[string]string{"database": "shop", "code": "75"}); err == nil || errors.Is(err, pipeline.ErrPermanent) {
		t.Errorf("exit 75 deve essere temporaneo: %v", err)
	}
	if err := run(ps, map[string]string{"database": "shop", "code": "3"}); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "code 3") {
		t.Errorf("exit 3 deve essere permanente: %v", err)
	}

	// Signature, revocation and ownership of the spec.
	bad := ps
	bad.SpecMAC = strings.Repeat("0", 64)
	if err := run(bad, ok); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "signature") {
		t.Errorf("firma non valida: %v", err)
	}
	moved := ps
	moved.Name = "other"
	if err := run(moved, ok); !errors.Is(err, pipeline.ErrPermanent) {
		t.Errorf("specifica di un altro step: %v", err)
	}
	f.b.Revoked = []string{ps.SpecHash}
	if err := run(ps, ok); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("specifica revocata: %v", err)
	}
	f.b.Revoked = nil

	// Binary verified with the sha256.
	data, _ := os.ReadFile(f.tool)
	sum := sha256.Sum256(data)
	s := f.commandSpec()
	s.Run.SHA256 = hex.EncodeToString(sum[:])
	if err := run(f.planned(t, "db-migrate", s), ok); err != nil {
		t.Errorf("sha256 corretto: %v", err)
	}
	s.Run.SHA256 = strings.Repeat("a", 64)
	if err := run(f.planned(t, "db-migrate", s), ok); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("sha256 errato: %v", err)
	}

	// Undeclared outputs or outputs with secrets are rejected.
	s = f.commandSpec()
	s.Outputs = map[string]customspec.Output{"other": {Pattern: ".*"}}
	if err := run(f.planned(t, "db-migrate", s), ok); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("output non dichiarato: %v", err)
	}

	// Version not usable in placeholders.
	in := input(ok)
	in.Descriptor.Artifact.Version = "1.0 --evil"
	if _, err := f.b.Build(ps, "").Run(context.Background(), in); !errors.Is(err, pipeline.ErrPermanent) {
		t.Errorf("versione non valida: %v", err)
	}

	// Secret not declared in the effective descriptor.
	in = input(ok)
	in.Descriptor.Secrets = in.Descriptor.Secrets[1:]
	if _, err := f.b.Build(ps, "").Run(context.Background(), in); !errors.Is(err, pipeline.ErrPermanent) || !strings.Contains(err.Error(), "db-admin") {
		t.Errorf("segreto non dichiarato: %v", err)
	}

	failed := 0
	for _, e := range f.audit.entries {
		if e.Outcome == audit.OutcomeFailed {
			failed++
		}
	}
	if failed == 0 {
		t.Error("gli errori di esecuzione non sono nell'audit")
	}
}

func TestComandoSuTuttiITarget(t *testing.T) {
	f := newFixture(t)
	s := f.commandSpec()
	s.Run.RunOnce, s.Outputs = false, nil
	ps := f.planned(t, "db-migrate", s)
	out, err := f.b.Build(ps, "").Run(context.Background(), input(map[string]string{"database": "shop"}))
	if err != nil || len(out) != 0 {
		t.Fatalf("esecuzione: %v, %v", out, err)
	}
	if got := strings.Join(f.dial.dials, ","); got != "db1:deploy:PRIVATE-KEY,db2:deploy:PRIVATE-KEY" {
		t.Errorf("target: %s", got)
	}
	if e := f.audit.entries[0]; strings.Join(e.Targets, ",") != "db1,db2" {
		t.Errorf("target nell'audit: %v", e.Targets)
	}
}

func TestHTTP(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var got []string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization")+" "+r.Header.Get(HeaderIdempotencyKey)+" "+string(body))
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/zones/"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"result": {"id": "p-1"}}`)
		case r.URL.Path == "/purges/p-1":
			polls++
			status := "running"
			if polls >= 2 {
				status = "done"
			}
			fmt.Fprintf(w, `{"status": %q, "result": {"id": "p-1"}}`, status)
		case r.URL.Path == "/fail":
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/bad":
			w.WriteHeader(http.StatusBadRequest)
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "https://evil.example.com/", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)

	spec := func(url string) customspec.Spec {
		s := customspec.Spec{
			Type: customspec.TypeHTTP, Secrets: []string{"cdn-token"},
			Params:  map[string]customspec.Param{"zone": {Pattern: "[^\\n]+"}},
			Outputs: map[string]customspec.Output{"purge_id": {Pattern: "[a-z0-9-]+", Path: "$.result.id"}},
			Run: &customspec.Action{
				URL:  url,
				Auth: &customspec.Auth{Type: "bearer", Secret: "cdn-token"},
				JSON: map[string]any{"tags": []any{"{{ .Application }}"}, "reason": "{{ .Params.zone }}", "n": json.Number("12345678901234567890")},
			},
		}
		s.ApplyDefaults()
		return s
	}
	in := func(zone string) pipeline.Input {
		i := input(nil)
		i.Descriptor.StepParams = map[string]map[string]string{"cdn-purge": {"zone": zone}}
		return i
	}
	s := spec(srv.URL + "/zones/{{ .Params.zone }}/purge")
	s.Run.Poll = &customspec.Poll{URL: srv.URL + "/purges/{{ .Response.result.id }}", Interval: time.Second, Timeout: 10 * time.Second,
		Until: customspec.Condition{Path: "$.status", In: []string{"done"}}, Fail: &customspec.Condition{Path: "$.status", In: []string{"failed"}}}
	ps := f.planned(t, "cdn-purge", s)
	out, err := f.b.Build(ps, "oidc:alice").Run(context.Background(), in(`a/b?c=1&x"y`))
	if err != nil {
		t.Fatalf("esecuzione: %v\n%s", err, f.logs)
	}
	if out["purge_id"] != "p-1" {
		t.Errorf("output: %v", out)
	}
	// The value stays a single path segment and a string of the JSON body.
	want := `POST /zones/a%2Fb%3Fc%3D1%26x%22y/purge Bearer cdn-token-value shop-1.4.0-prod:cdn-purge {"n":12345678901234567890,"reason":"a/b?c=1\u0026x\"y","tags":["shop"]}`
	if got[0] != want {
		t.Errorf("richiesta:\n%s\natteso:\n%s", got[0], want)
	}
	if polls != 2 || !strings.HasPrefix(got[len(got)-1], "GET /purges/p-1 Bearer") {
		t.Errorf("attesa: %d letture, %v", polls, got)
	}
	if strings.Contains(f.logs.String(), "cdn-token-value") {
		t.Error("il token compare nel log")
	}

	cases := map[string]struct {
		url, zone string
		permanent bool
		want      string
	}{
		"traversal":    {srv.URL + "/zones/{{ .Params.zone }}/purge", "..", true, "path segment"},
		"servizio giù": {srv.URL + "/fail", "z", false, "503"},
		"rifiuto":      {srv.URL + "/bad", "z", true, "400"},
		"redirect":     {srv.URL + "/redirect", "z", true, "302"},
	}
	for name, c := range cases {
		_, err := f.b.Build(f.planned(t, "cdn-purge", spec(c.url)), "").Run(context.Background(), in(c.zone))
		if err == nil || errors.Is(err, pipeline.ErrPermanent) != c.permanent || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRedazione(t *testing.T) {
	r := newRedactor([]string{"topsecret\n", "abc", "-----BEGIN KEY-----\nMIIEvQIBADANBgkqhkiG9w0\n-----END KEY-----"})
	for _, in := range []string{"topsecret", "dG9wc2VjcmV0", "x=topsecret;", "MIIEvQIBADANBgkqhkiG9w0"} {
		if out := r.apply(in); strings.Contains(out, "topsecret") || strings.Contains(out, "dG9wc2VjcmV0") || strings.Contains(out, "MIIEvQ") {
			t.Errorf("%q → %q", in, out)
		}
	}
	if out := r.apply("abc def"); out != "abc def" {
		t.Errorf("i valori troppo corti non vanno redatti: %q", out)
	}
	if !r.contains("a topsecret b") || r.contains("nulla") {
		t.Error("contains")
	}
}

func TestQuotatura(t *testing.T) {
	for _, s := range []string{"", "a b", "it's", `"$HOME"`, "$(id)", "`id`", "a\\b", "'''"} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("Quote(%q): %q, %v", s, out, err)
		}
	}
}
