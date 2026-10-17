package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

var (
	labelVersionRe = regexp.MustCompile(`'manent\.version=([^']*)'`)
	imageArgRe     = regexp.MustCompile(`'([^' ]+@sha256:[a-f0-9]{64})'`)
)

// fakeHost simulates a target's container runtime.
type fakeHost struct {
	version, image string // container in service ("" = none)
	state          string // answer to the wait (default "running||0")
	failPull       bool
	commands       []string
	stdin          []string
}

func (h *fakeHost) Upload(context.Context, string, string) error { return nil }
func (h *fakeHost) Close() error                                 { return nil }

func (h *fakeHost) RunStdin(ctx context.Context, cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	h.stdin = append(h.stdin, string(b))
	return h.Run(ctx, cmd)
}

func (h *fakeHost) Run(_ context.Context, cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "if ") && strings.Contains(cmd, "container inspect"):
		if h.image == "" {
			return "", nil
		}
		status := "running"
		if h.state != "" {
			status, _, _ = strings.Cut(h.state, "|")
		}
		return h.version + "|" + h.image + "|" + status + "\n", nil
	case strings.Contains(cmd, " pull "):
		if h.failPull {
			return "", errors.New("manifest unknown")
		}
		return "Status: Downloaded newer image\n", nil
	case strings.Contains(cmd, " run -d "):
		h.version = labelVersionRe.FindStringSubmatch(cmd)[1]
		h.image = imageArgRe.FindStringSubmatch(cmd)[1]
		return "c0ffee\n", nil
	case strings.Contains(cmd, "container inspect -f"):
		if h.state != "" {
			return h.state + "\n", nil
		}
		return "running||0\n", nil
	case strings.Contains(cmd, " version --format"):
		return "27.3.1\n", nil
	case strings.Contains(cmd, " logs "):
		return "panic: configurazione mancante\n", nil
	}
	return "", nil
}

type fakeDialer struct{ hosts map[string]*fakeHost }

func (d fakeDialer) Dial(_ context.Context, t plugin.Target, _ remotecopy.Auth) (remotecopy.Session, error) {
	return d.hosts[t.Host], nil
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", errors.New("segreto non trovato")
}

const digest1 = "1111111111111111111111111111111111111111111111111111111111111111"

func imageFile(t *testing.T, ref string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shop"+plugin.ImageRefExt)
	if err := os.WriteFile(p, []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newPlugin(hosts map[string]*fakeHost) *Plugin {
	p := New(fakeDialer{hosts: hosts}, []string{"container"}, slog.New(slog.DiscardHandler))
	p.poll = 10 * time.Millisecond
	return p
}

func newRequest(t *testing.T, hosts ...string) *plugin.Request {
	req := &plugin.Request{
		DeploymentID: "d1", Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "container", Name: "shop", Version: "1.1.0", SHA256: digest1,
			LocalPath: imageFile(t, "registry:5000/team/shop@sha256:"+digest1)},
		Params:  map[string]string{ParamSSHKeySecret: "key", ParamRunArgs: "-p 8080:8080", ParamStableFor: "20ms"},
		Secrets: mapSecrets{"key": "PEM", "reg": "pw"},
	}
	for _, h := range hosts {
		req.Targets = append(req.Targets, plugin.Target{Host: h, User: "deploy"})
	}
	return req
}

func TestDeploySostituisceIlContainer(t *testing.T) {
	old := "registry:5000/team/shop@sha256:" + strings.Repeat("0", 64)
	hosts := map[string]*fakeHost{"vm1": {version: "1.0.0", image: old}, "vm2": {}}
	p := newPlugin(hosts)
	req := newRequest(t, "vm1", "vm2")
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Targets) != 2 || res.Targets[0].Details["previous_version"] != "1.0.0" || res.Targets[0].Details["previous_image"] != old {
		t.Fatalf("esito inatteso: %+v", res)
	}
	for name, h := range hosts {
		if h.version != "1.1.0" || !strings.HasSuffix(h.image, digest1) {
			t.Errorf("%s: container %s %s", name, h.version, h.image)
		}
	}
	var run string
	for _, c := range hosts["vm1"].commands {
		if strings.Contains(c, " run -d ") {
			run = c
		}
	}
	if !strings.Contains(run, "docker rm -f 'shop'") || !strings.Contains(run, "-p 8080:8080 'registry:5000/team/shop@sha256:"+digest1+"'") {
		t.Errorf("comando di avvio inatteso: %s", run)
	}

	// Run again (retry or recovery) it does not restart the container.
	runs := func() int {
		n := 0
		for _, c := range hosts["vm1"].commands {
			if strings.Contains(c, " run -d ") {
				n++
			}
		}
		return n
	}
	n := runs()
	res, err = p.Deploy(context.Background(), req)
	if err != nil || res.Targets[0].Details["unchanged"] != "true" || runs() != n {
		t.Errorf("deploy non idempotente: %+v %v", res, err)
	}

	// The release in service is read from the labels, with the image for the rollback.
	rel, err := p.CurrentRelease(context.Background(), req)
	if err != nil || rel.Version != "1.1.0" || !strings.HasSuffix(rel.Details[DetailImage], digest1) {
		t.Errorf("release in esercizio: %+v %v", rel, err)
	}
	if _, err := p.Rollback(context.Background(), req, plugin.Release{Version: "1.0.0", Details: map[string]string{DetailImage: old}}); err != nil {
		t.Fatal(err)
	}
	if hosts["vm2"].version != "1.0.0" || hosts["vm2"].image != old {
		t.Errorf("rollback non applicato: %+v", hosts["vm2"])
	}
	hosts["vm1"].version = "9.9.9"
	if _, err := p.CurrentRelease(context.Background(), req); err == nil {
		t.Error("attese release diverse tra i target")
	}
}

func TestDeployFallito(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*fakeHost{"vm1": {state: "exited||0"}}
	p := newPlugin(hosts)
	req := newRequest(t, "vm1")
	if _, err := p.Deploy(ctx, req); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("atteso container terminato: %v", err)
	}
	hosts["vm1"].state = "running|unhealthy|0"
	if _, err := p.Deploy(ctx, req); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("atteso unhealthy: %v", err)
	}
	hosts["vm1"].state, hosts["vm1"].failPull, hosts["vm1"].image = "", true, ""
	if _, err := p.Deploy(ctx, req); err == nil || !strings.Contains(err.Error(), "pull") {
		t.Fatalf("atteso errore di pull: %v", err)
	}
	// Continuous restarts: never stable within the timeout.
	hosts["vm1"].failPull, hosts["vm1"].image, hosts["vm1"].state = false, "", "restarting||3"
	req.Params[ParamWaitTimeout] = "100ms"
	if _, err := p.Deploy(ctx, req); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("atteso timeout: %v", err)
	}
}

func TestLoginEValidazione(t *testing.T) {
	hosts := map[string]*fakeHost{"vm1": {}}
	p := newPlugin(hosts)
	req := newRequest(t, "vm1")
	req.Params[ParamRegistryUser], req.Params[ParamRegistrySecret], req.Params[ParamRuntime] = "ci", "reg", "podman"
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h := hosts["vm1"]
	if len(h.stdin) != 1 || h.stdin[0] != "pw" || !strings.Contains(h.commands[1], "podman login -u 'ci' --password-stdin 'registry:5000'") {
		t.Errorf("login inatteso: %v %v", h.stdin, h.commands)
	}
	for _, c := range h.commands {
		if strings.Contains(c, "pw") {
			t.Errorf("password nella riga di comando: %s", c)
		}
	}

	bad := newRequest(t, "vm1")
	bad.Params[ParamRuntime], bad.Params[ParamContainerName], bad.Params[ParamWaitTimeout] = "lxc", "../x", "presto"
	bad.Params[ParamRegistryUser] = "ci"
	bad.Artifact.LocalPath, bad.Artifact.Version = "/tmp/app.tar.gz", ""
	bad.Targets[0].User = ""
	err := p.Validate(bad)
	for _, want := range []string{"runtime", "container_name", "artifact.version", "registry_password_secret", "registry image", "wait_timeout", "user is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validazione: manca %q in %v", want, err)
		}
	}
}

func TestDryRun(t *testing.T) {
	hosts := map[string]*fakeHost{"vm1": {version: "1.0.0", image: "r/x@sha256:" + digest1}}
	p := newPlugin(hosts)
	req := newRequest(t, "vm1")
	res, err := p.DryRun(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Targets[0].Details
	if res.Targets[0].Status != plugin.StatusReady || d["runtime_version"] != "27.3.1" || d["current_version"] != "1.0.0" || d["pull"] != "skipped" {
		t.Errorf("dry-run inatteso: %+v", res)
	}
	for _, c := range hosts["vm1"].commands {
		if strings.Contains(c, " pull ") || strings.Contains(c, " run ") || strings.Contains(c, " rm ") {
			t.Errorf("il dry-run ha modificato il target: %s", c)
		}
	}
}
