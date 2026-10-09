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

// fakeSession registra le operazioni eseguite e simula sha256sum.
type fakeSession struct {
	uploads  []string
	commands []string
	checksum string
}

func (f *fakeSession) Upload(_ context.Context, _, remote string) error {
	f.uploads = append(f.uploads, remote)
	return nil
}

func (f *fakeSession) Run(_ context.Context, cmd string) (string, error) {
	f.commands = append(f.commands, cmd)
	if strings.HasPrefix(cmd, "sha256sum") {
		return f.checksum + "  file\n", nil
	}
	return "", nil
}

func (f *fakeSession) Close() error { return nil }

type fakeDialer struct {
	sess *fakeSession
	auth Auth
}

func (d *fakeDialer) Dial(_ context.Context, _ plugin.Target, a Auth) (Session, error) {
	d.auth = a
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
