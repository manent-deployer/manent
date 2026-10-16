package remotecopy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// fakeSession records the operations run and simulates sha256sum.
type fakeSession struct {
	uploads  []string
	commands []string
	checksum string
	// current is the target of the current symlink ("" = none).
	current string
	// missing makes "test -d" fail (release missing).
	missing bool
}

func (f *fakeSession) Upload(_ context.Context, _, remote string) error {
	f.uploads = append(f.uploads, remote)
	return nil
}

func (f *fakeSession) Run(_ context.Context, cmd string) (string, error) {
	f.commands = append(f.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "sha256sum"):
		return f.checksum + "  file\n", nil
	case strings.Contains(cmd, "readlink"):
		return f.current + "\n", nil
	case strings.HasPrefix(cmd, "test -d") && f.missing:
		return "", errors.New("exit status 1")
	}
	return "", nil
}

func (f *fakeSession) Close() error { return nil }

type fakeDialer struct {
	sess *fakeSession
	// hosts, if set, assigns a session to every host.
	hosts map[string]*fakeSession
	auth  Auth
}

func (d *fakeDialer) Dial(_ context.Context, t plugin.Target, a Auth) (Session, error) {
	d.auth = a
	if s, ok := d.hosts[t.Host]; ok {
		return s, nil
	}
	return d.sess, nil
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", errors.New("non trovato")
	}
	return v, nil
}

func newRequest() *plugin.Request {
	return &plugin.Request{
		DeploymentID: "d1",
		Artifact:     plugin.Artifact{Type: "application", LocalPath: "/tmp/work/app.tar.gz", SHA256: "abc"},
		Targets:      []plugin.Target{{Host: "h1", User: "deploy"}},
		Params:       map[string]string{ParamDestDir: "/opt/app", ParamSSHKeySecret: "key", ParamPostCommand: "systemctl restart app"},
		Secrets:      mapSecrets{"key": "PEM"},
	}
}

func TestDeploy(t *testing.T) {
	sess := &fakeSession{checksum: "abc"}
	d := &fakeDialer{sess: sess}
	p := New(d, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := newRequest()

	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.auth.PrivateKey != "PEM" {
		t.Errorf("chiave non risolta dal segreto")
	}
	if len(sess.uploads) != 1 || sess.uploads[0] != "/opt/app/app.tar.gz" {
		t.Errorf("upload inatteso: %v", sess.uploads)
	}
	if got := sess.commands[len(sess.commands)-1]; got != "systemctl restart app" {
		t.Errorf("post_command non eseguito, ultimo comando: %q", got)
	}
	if res.Targets[0].Status != plugin.StatusDeployed || res.Targets[0].Details["checksum"] != "verified" {
		t.Errorf("risultato inatteso: %+v", res.Targets[0])
	}
}

func TestDeployChecksumErrato(t *testing.T) {
	p := New(&fakeDialer{sess: &fakeSession{checksum: "zzz"}}, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := p.Deploy(context.Background(), newRequest())
	if err == nil {
		t.Fatal("atteso errore di checksum")
	}
	if res.Targets[0].Status != plugin.StatusFailed {
		t.Errorf("stato atteso %q", plugin.StatusFailed)
	}
}

func TestValidate(t *testing.T) {
	p := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := newRequest()
	req.Params = map[string]string{}
	req.Targets[0].User = ""
	if err := p.Validate(req); err == nil {
		t.Fatal("attesi errori di validazione")
	}
}

func TestDryRun(t *testing.T) {
	sess := &fakeSession{}
	p := New(&fakeDialer{sess: sess}, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := p.DryRun(context.Background(), newRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.uploads) != 0 {
		t.Errorf("il dry-run non deve copiare: %v", sess.uploads)
	}
	for _, c := range sess.commands {
		if strings.Contains(c, "mkdir") || c == "systemctl restart app" {
			t.Errorf("comando con effetti eseguito in dry-run: %q", c)
		}
	}
	tr := res.Targets[0]
	if tr.Status != plugin.StatusReady || tr.Details["dest_dir"] != "writable" || tr.Details["post_command"] != "skipped" {
		t.Errorf("risultato inatteso: %+v", tr)
	}
}

func newReleasesRequest() *plugin.Request {
	req := newRequest()
	req.Artifact.Version = "1.2.0"
	req.Params[ParamLayout] = LayoutReleases
	req.Params[ParamKeepReleases] = "3"
	req.Params[ParamPrepareCommand] = `tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"`
	return req
}

// indexOf returns the index of the first command containing sub, or -1.
func indexOf(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func TestDeployReleases(t *testing.T) {
	sess := &fakeSession{checksum: "abc", current: "releases/1.1.0"}
	p := New(&fakeDialer{sess: sess}, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := newReleasesRequest()
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.uploads) != 1 || sess.uploads[0] != "/opt/app/releases/1.2.0/app.tar.gz" {
		t.Errorf("upload inatteso: %v", sess.uploads)
	}
	prepare, sw := indexOf(sess.commands, "tar -xzf"), indexOf(sess.commands, "ln -sfn 'releases/1.2.0'")
	post, prune := indexOf(sess.commands, "systemctl restart app"), indexOf(sess.commands, "ls -1t")
	if prepare < 0 || sw < prepare || post < sw || prune < post {
		t.Fatalf("ordine dei comandi inatteso: %q", sess.commands)
	}
	if c := sess.commands[post]; !strings.Contains(c, "MANENT_RELEASE_DIR='/opt/app/releases/1.2.0'") {
		t.Errorf("post_command senza variabili della release: %q", c)
	}
	// Cleanup keeps the activated release and the previous one, plus one more (keep_releases=3).
	if c := sess.commands[prune]; !strings.Contains(c, "-e '1.2.0' -e '1.1.0'") || !strings.Contains(c, "tail -n +2") {
		t.Errorf("pulizia inattesa: %q", c)
	}
	if d := res.Targets[0].Details; d["release"] != "1.2.0" || d["previous"] != "1.1.0" {
		t.Errorf("dettagli inattesi: %v", d)
	}
}

func TestValidateReleases(t *testing.T) {
	p := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, mod := range map[string]func(*plugin.Request){
		"versione mancante":    func(r *plugin.Request) { r.Artifact.Version = "" },
		"versione non valida":  func(r *plugin.Request) { r.Artifact.Version = "../x" },
		"keep_releases < 2":    func(r *plugin.Request) { r.Params[ParamKeepReleases] = "1" },
		"layout sconosciuto":   func(r *plugin.Request) { r.Params[ParamLayout] = "tree" },
		"prepare_command flat": func(r *plugin.Request) { r.Params[ParamLayout] = LayoutFlat },
	} {
		req := newReleasesRequest()
		mod(req)
		if err := p.Validate(req); err == nil {
			t.Errorf("%s: atteso errore di validazione", name)
		}
	}
}

func TestCurrentRelease(t *testing.T) {
	d := &fakeDialer{hosts: map[string]*fakeSession{
		"h1": {current: "releases/1.1.0"},
		"h2": {current: "/opt/app/releases/1.1.0"},
		"h3": {}, // new target, without a release: ignored
	}}
	p := New(d, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := newReleasesRequest()
	req.Targets = []plugin.Target{{Host: "h1", User: "u"}, {Host: "h2", User: "u"}, {Host: "h3", User: "u"}}
	rel, err := p.CurrentRelease(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil || rel.Version != "1.1.0" || rel.Details["targets"] != "h1,h2" {
		t.Fatalf("release inattesa: %+v", rel)
	}

	d.hosts["h2"].current = "releases/1.0.0"
	if _, err := p.CurrentRelease(context.Background(), req); err == nil || !strings.Contains(err.Error(), "release diverse") {
		t.Errorf("atteso errore per release diverse, ricevuto %v", err)
	}

	// No target with current: first deploy.
	req.Targets = req.Targets[2:]
	if rel, err := p.CurrentRelease(context.Background(), req); err != nil || rel != nil {
		t.Errorf("attesa nessuna release: %+v %v", rel, err)
	}

	// The flat layout has no current release.
	req.Params[ParamLayout] = LayoutFlat
	if _, err := p.CurrentRelease(context.Background(), req); err == nil {
		t.Error("atteso errore con layout flat")
	}
}

func TestRollback(t *testing.T) {
	ok, missing := &fakeSession{}, &fakeSession{missing: true}
	d := &fakeDialer{hosts: map[string]*fakeSession{"h1": ok, "h2": missing}}
	p := New(d, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := newReleasesRequest()
	req.Params[ParamRollbackCommand] = "systemctl reload app"
	req.Targets = []plugin.Target{{Host: "h1", User: "u"}, {Host: "h2", User: "u"}}

	res, err := p.Rollback(context.Background(), req, plugin.Release{Version: "1.1.0"})
	// h2 lacks the release: an error, but h1 is restored anyway.
	if err == nil || !strings.Contains(err.Error(), "h2") {
		t.Fatalf("atteso errore su h2, ricevuto %v", err)
	}
	if res.Targets[0].Status != plugin.StatusRolledBack || res.Targets[1].Status != plugin.StatusFailed {
		t.Fatalf("esiti inattesi: %+v", res.Targets)
	}
	sw, cmd := indexOf(ok.commands, "ln -sfn 'releases/1.1.0'"), indexOf(ok.commands, "systemctl reload app")
	if sw < 0 || cmd < sw || indexOf(ok.commands, "systemctl restart app") >= 0 {
		t.Errorf("comandi inattesi su h1: %q", ok.commands)
	}
	if indexOf(missing.commands, "ln -sfn") >= 0 {
		t.Errorf("switch eseguito su un target senza la release: %q", missing.commands)
	}
}
