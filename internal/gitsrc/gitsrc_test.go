package gitsrc

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// fixture is a source repository built with the git binary.
type fixture struct {
	t   *testing.T
	dir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &fixture{t: t, dir: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	return f
}

// git runs a git command in the fixture, with an isolated configuration.
func (f *fixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the files and commits them, returning the SHA.
func (f *fixture) commit(msg string, files map[string]string, extra ...string) string {
	f.t.Helper()
	for name, content := range files {
		p := filepath.Join(f.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.git("add", "-A")
	f.git(append([]string{"commit", "-q", "-m", msg}, extra...)...)
	return f.git("rev-parse", "HEAD")
}

func newRepo(t *testing.T, opt Options) *Repo {
	t.Helper()
	if opt.CacheDir == "" {
		opt.CacheDir = t.TempDir()
	}
	r, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRefSHAELettura(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	first := f.commit("v1", map[string]string{"shop/shop/prod.yml": "plugin: a\n"})
	f.git("tag", "-a", "env-1", "-m", "env 1")
	f.git("checkout", "-q", "-b", "feature")
	feature := f.commit("feature", map[string]string{"shop/shop/prod.yml": "plugin: evil\n"})
	f.git("checkout", "-q", "main")
	second := f.commit("v2", map[string]string{"shop/shop/prod.yml": "plugin: b\n"})

	r := newRepo(t, Options{Repository: f.dir, DefaultRef: "refs/heads/main", AllowedRefs: []string{"refs/tags/env-*"}})
	c, err := r.Resolve(ctx, "", false)
	if err != nil || c.SHA != second || c.Ref != "refs/heads/main" {
		t.Fatalf("default ref: %+v %v", c, err)
	}
	data, err := r.Read(ctx, c.SHA, "shop/shop/prod.yml")
	if err != nil || string(data) != "plugin: b\n" {
		t.Fatalf("read: %q %v", data, err)
	}
	// An annotated tag is peeled to its commit; short names are expanded.
	if c, err := r.Resolve(ctx, "env-1", false); err != nil || c.SHA != first || c.Ref != "refs/tags/env-1" {
		t.Fatalf("tag: %+v %v", c, err)
	}
	// A SHA reachable from main is accepted, the old content is read.
	if c, err := r.Resolve(ctx, first, false); err != nil || c.SHA != first || c.Ref != "" {
		t.Fatalf("sha: %+v %v", c, err)
	}
	if data, _ := r.Read(ctx, first, "shop/shop/prod.yml"); string(data) != "plugin: a\n" {
		t.Fatalf("old content: %q", data)
	}
	// Refs and SHAs outside the allowed refs are refused, permanently.
	for _, want := range []string{"feature", "refs/heads/feature", feature} {
		_, err := r.Resolve(ctx, want, false)
		if err == nil || pipeline.Retryable(err) {
			t.Fatalf("%s: expected a permanent error, got %v", want, err)
		}
	}
	if _, err := r.Resolve(ctx, "../x", false); err == nil || !strings.Contains(err.Error(), "invalid config ref") {
		t.Fatalf("invalid ref: %v", err)
	}
	// A missing file is ErrNotFound; a path escaping the root is refused.
	if _, err := r.Read(ctx, second, "shop/shop/test.yml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.Read(ctx, second, "../etc/passwd"); err == nil || pipeline.Retryable(err) {
		t.Fatalf("escape: %v", err)
	}
}

func TestRefTTLEBranchCancellato(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.commit("v1", map[string]string{"a.yml": "x\n"})
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	r := newRepo(t, Options{Repository: f.dir, AllowedRefs: []string{"refs/heads/release-*"}, RefTTL: 30 * time.Second, Now: func() time.Time { return now }})
	if _, err := r.Resolve(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	second := f.commit("v2", map[string]string{"a.yml": "y\n"})
	// Within ref_ttl the previous resolution is reused, after it the new commit is seen.
	if c, _ := r.Resolve(ctx, "", false); c.SHA == second {
		t.Fatal("fetched within ref_ttl")
	}
	now = now.Add(time.Minute)
	if c, _ := r.Resolve(ctx, "", false); c.SHA != second {
		t.Fatalf("ref_ttl expired: %s", c.SHA)
	}
	// A commit only on a deleted allowed branch stops being reachable.
	f.git("checkout", "-q", "-b", "release-1")
	only := f.commit("rel", map[string]string{"a.yml": "z\n"})
	f.git("checkout", "-q", "main")
	if _, err := r.Resolve(ctx, only, false); err != nil {
		t.Fatalf("on release-1: %v", err)
	}
	f.git("branch", "-q", "-D", "release-1")
	now = now.Add(time.Minute)
	if _, err := r.Resolve(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, only, false); err == nil {
		t.Fatal("commit of a deleted branch still reachable")
	}
}

func TestMaxAge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	old := f.commit("old", map[string]string{"a.yml": "x\n"})
	f.commit("new", map[string]string{"a.yml": "y\n"})
	later := time.Now().Add(48 * time.Hour)
	r := newRepo(t, Options{Repository: f.dir, MaxAge: 24 * time.Hour, Now: func() time.Time { return later }})
	if _, err := r.Resolve(ctx, old, false); err == nil || !strings.Contains(err.Error(), "older than max_age") {
		t.Fatalf("max_age: %v", err)
	}
	// A pinned revision (rollback) is exempt.
	if _, err := r.Resolve(ctx, old, true); err != nil {
		t.Fatalf("pinned: %v", err)
	}
}

func TestFirmaSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	ctx := context.Background()
	f := newFixture(t)
	keys := t.TempDir()
	for _, k := range []string{"good", "other"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(keys, k)).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
	}
	pub, _ := os.ReadFile(filepath.Join(keys, "good.pub"))
	signers := filepath.Join(keys, "allowed_signers")
	if err := os.WriteFile(signers, []byte("platform@example.com namespaces=\"git\" "+string(pub)), 0o644); err != nil {
		t.Fatal(err)
	}
	sign := func(key string) []string {
		return []string{"-c", "gpg.format=ssh", "-c", "user.signingkey=" + filepath.Join(keys, key)}
	}
	unsigned := f.commit("unsigned", map[string]string{"a.yml": "x\n"})
	f.git(append(sign("other"), "commit", "-q", "--allow-empty", "-S", "-m", "other")...)
	other := f.git("rev-parse", "HEAD")
	f.git(append(sign("good"), "commit", "-q", "--allow-empty", "-S", "-m", "good")...)
	good := f.git("rev-parse", "HEAD")

	r := newRepo(t, Options{Repository: f.dir, RequireSigned: true, SSHAllowedSigners: signers})
	if c, err := r.Resolve(ctx, "", false); err != nil || c.SHA != good {
		t.Fatalf("signed by an allowed key: %+v %v", c, err)
	}
	if _, err := r.Resolve(ctx, other, false); err == nil || !strings.Contains(err.Error(), "not in signers.ssh_allowed_signers") {
		t.Fatalf("other key: %v", err)
	}
	if _, err := r.Resolve(ctx, unsigned, false); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned: %v", err)
	}
	if _, err := New(Options{Repository: f.dir, CacheDir: t.TempDir(), RequireSigned: true}); err == nil {
		t.Fatal("require_signed without signers accepted")
	}
}

// fakeSecrets resolves references from a map.
type fakeSecrets map[string]string

func (s fakeSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := s[ref]; ok {
		return v, nil
	}
	return "", errors.New("unknown secret " + ref)
}

func TestHTTPConCredenziali(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	sha := f.commit("v1", map[string]string{"a.yml": "x\n"})
	bare := filepath.Join(t.TempDir(), "envconfig.git")
	f.git("clone", "-q", "--bare", f.dir, bare)
	gitPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path")
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(gitPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "manent" || p != "tok" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer srv.Close()

	r := newRepo(t, Options{Repository: srv.URL + "/envconfig.git", Username: "manent", PasswordRef: "file:/tok", Secrets: fakeSecrets{"file:/tok": "tok"}})
	if c, err := r.Resolve(ctx, "", false); err != nil || c.SHA != sha {
		t.Fatalf("http: %+v %v", c, err)
	}
	if r.URL() != srv.URL+"/envconfig.git" {
		t.Fatalf("url: %s", r.URL())
	}
	bad := newRepo(t, Options{Repository: srv.URL + "/envconfig.git", Username: "manent", PasswordRef: "file:/tok", Secrets: fakeSecrets{"file:/tok": "wrong"}})
	if _, err := bad.Resolve(ctx, "", false); err == nil || pipeline.Retryable(err) {
		t.Fatalf("wrong token: expected a permanent error, got %v", err)
	}
	// An unreachable server is transient.
	down := newRepo(t, Options{Repository: "http://127.0.0.1:1/envconfig.git"})
	if _, err := down.Resolve(ctx, "", false); err == nil || !pipeline.Retryable(err) {
		t.Fatalf("unreachable: expected a transient error, got %v", err)
	}
}

func TestRedact(t *testing.T) {
	if got := redact("https://user:tok@git.example.com/a.git"); got != "https://git.example.com/a.git" {
		t.Fatal(got)
	}
	if got := redact("/srv/git/a"); got != "/srv/git/a" {
		t.Fatal(got)
	}
}

func TestFirmaOpenPGP(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	ctx := context.Background()
	f := newFixture(t)
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	gpg := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("gpg", append([]string{"--batch", "--homedir", home}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gpg %v: %v", args, err)
		}
		return out
	}
	gpg("--passphrase", "", "--quick-gen-key", "platform@example.com", "ed25519", "sign", "never")
	keyring := filepath.Join(t.TempDir(), "keys.asc")
	if err := os.WriteFile(keyring, gpg("--armor", "--export", "platform@example.com"), 0o644); err != nil {
		t.Fatal(err)
	}
	unsigned := f.commit("unsigned", map[string]string{"a.yml": "x\n"})
	t.Setenv("GNUPGHOME", home)
	f.git("-c", "user.signingkey=platform@example.com", "commit", "-q", "--allow-empty", "-S", "-m", "signed")
	signed := f.git("rev-parse", "HEAD")

	r := newRepo(t, Options{Repository: f.dir, RequireSigned: true, OpenPGPKeyring: keyring})
	if c, err := r.Resolve(ctx, "", false); err != nil || c.SHA != signed {
		t.Fatalf("signed: %+v %v", c, err)
	}
	if _, err := r.Resolve(ctx, unsigned, false); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("unsigned: %v", err)
	}
}
