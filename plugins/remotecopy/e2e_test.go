package remotecopy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// E2E tests use a real SSH server: scripts/e2e-ssh.sh starts one in Docker and sets
// MANENT_TEST_SSH_ADDR (host:port), MANENT_TEST_SSH_USER and MANENT_TEST_SSH_KEY (private key).
func e2eTarget(t *testing.T) (plugin.Target, mapSecrets) {
	t.Helper()
	addr := os.Getenv("MANENT_TEST_SSH_ADDR")
	if addr == "" {
		t.Skip("MANENT_TEST_SSH_ADDR non impostata: eseguire scripts/e2e-ssh.sh")
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(p)
	key, err := os.ReadFile(os.Getenv("MANENT_TEST_SSH_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Target{Host: host, Port: port, User: os.Getenv("MANENT_TEST_SSH_USER")}, mapSecrets{"key": string(key)}
}

// e2eArtifact creates a tar.gz with a VERSION file and returns its path and sha256.
func e2eArtifact(t *testing.T, version string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte(version + "\n")
	_ = tw.WriteHeader(&tar.Header{Name: "VERSION", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	data, _ := os.ReadFile(path)
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func TestE2EReleasesERollback(t *testing.T) {
	target, secrets := e2eTarget(t)
	ctx := context.Background()
	p := New(&SSHDialer{InsecureIgnoreHostKey: true}, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	dest := "/home/" + target.User + "/e2e-" + hex.EncodeToString(b)

	request := func(version string) *plugin.Request {
		path, sum := e2eArtifact(t, version)
		return &plugin.Request{
			DeploymentID: "e2e-" + version,
			Artifact:     plugin.Artifact{Type: "application", Name: "app", Version: version, LocalPath: path, SHA256: sum},
			Targets:      []plugin.Target{target},
			Params: map[string]string{
				ParamDestDir: dest, ParamSSHKeySecret: "key", ParamLayout: LayoutReleases, ParamKeepReleases: "2",
				ParamPrepareCommand: `tar -xzf "$MANENT_ARTIFACT" -C "$MANENT_RELEASE_DIR"`,
				ParamPostCommand:    `cp "$MANENT_CURRENT/VERSION" '` + dest + `/attiva'`,
			},
			Secrets: secrets,
		}
	}
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		req := request(v)
		if err := p.Validate(req); err != nil {
			t.Fatal(err)
		}
		res, err := p.Deploy(ctx, req)
		if err != nil {
			t.Fatalf("deploy %s: %v", v, err)
		}
		if res.Targets[0].Status != plugin.StatusDeployed {
			t.Fatalf("deploy %s: %+v", v, res)
		}
	}
	req := request("1.2.0")
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.2.0" {
		t.Fatalf("release corrente: %+v %v", rel, err)
	}
	if _, err := p.Rollback(ctx, req, plugin.Release{Version: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	if rel, _ := p.CurrentRelease(ctx, req); rel == nil || rel.Version != "1.1.0" {
		t.Fatalf("release dopo il rollback: %+v", rel)
	}

	// Direct check on the server: active file, kept releases (keep_releases=2 plus the active one).
	sess, err := p.dialer.Dial(ctx, target, Auth{PrivateKey: secrets["key"]})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer func() { _, _ = sess.Run(ctx, "rm -rf '"+dest+"'") }()
	out, err := sess.Run(ctx, "cat '"+dest+"/attiva'; ls -1 '"+dest+"/releases' | sort | tr '\\n' ' '")
	if err != nil {
		t.Fatal(err)
	}
	// The rollback runs post_command (rollback_command default): the file goes back to 1.1.0.
	if !strings.HasPrefix(out, "1.1.0\n") || !strings.Contains(out, "1.1.0 1.2.0") || strings.Contains(out, "1.0.0 ") {
		t.Errorf("stato remoto inatteso: %q", out)
	}
}

func TestE2EChecksumErrato(t *testing.T) {
	target, secrets := e2eTarget(t)
	p := New(&SSHDialer{InsecureIgnoreHostKey: true}, []string{"application"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	path, _ := e2eArtifact(t, "1.0.0")
	req := &plugin.Request{
		DeploymentID: "e2e-checksum",
		Artifact:     plugin.Artifact{Type: "application", Name: "app", Version: "1.0.0", LocalPath: path, SHA256: strings.Repeat("0", 64)},
		Targets:      []plugin.Target{target},
		Params:       map[string]string{ParamDestDir: "/home/" + target.User + "/e2e-checksum", ParamSSHKeySecret: "key"},
		Secrets:      secrets,
	}
	if _, err := p.Deploy(context.Background(), req); err == nil || !strings.Contains(err.Error(), "remote checksum") {
		t.Fatalf("atteso errore di checksum, ricevuto %v", err)
	}
}
