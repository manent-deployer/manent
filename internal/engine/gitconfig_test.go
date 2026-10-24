package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/envconfig"
	"github.com/manent-deployer/manent/internal/gitsrc"
	"github.com/manent-deployer/manent/internal/testdb"
)

// gitConfigRepo is a repository of environment configurations built with the git binary.
type gitConfigRepo struct {
	t   *testing.T
	dir string
}

func newGitConfigRepo(t *testing.T) *gitConfigRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &gitConfigRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitConfigRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the configuration of app/app/<env> with the given color and returns the SHA.
func (r *gitConfigRepo) commit(color string, envs ...string) string {
	r.t.Helper()
	for _, env := range envs {
		p := filepath.Join(r.dir, "app", "app", env+".yml")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		cfg := "targets:\n  - host: " + env + "-1\nparams:\n  color: " + color + "\n"
		if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", color)
	return r.git("rev-parse", "HEAD")
}

// gitEngine starts a worker that reads the environment configuration from the repository.
func gitEngine(t *testing.T, dir, configRef string) (*Engine, *provisionStep) {
	t.Helper()
	repo, err := gitsrc.New(gitsrc.Options{Repository: dir, AllowedRefs: []string{"refs/tags/env-*"}, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	step := &provisionStep{seen: map[string]descriptor.Deploy{}}
	reg, planner := testRegistry(t, artifactStep{}, step)
	opts := workerOpts(testdb.URL(t), "w1", reg)
	resolver := envconfig.Resolver{
		Provider: &envconfig.GitProvider{Repo: repo, PathTemplate: "{application}/{artifact}/{environment}.yml"},
		Policy:   &envconfig.DescriptorPolicy{Targets: true, ConfigRef: configRef},
	}
	opts.Resolver = resolver
	w, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	e := NewService(ServiceOptions{Client: w.Client(), QueueName: "deployments", Planner: planner, Resolver: resolver, Logger: discard})
	w.SetService(e)
	if err := w.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Shutdown(5 * time.Second) })
	return e, step
}

func TestConfigurazioneDaGit(t *testing.T) {
	ctx := context.Background()
	repo := newGitConfigRepo(t)
	blue := repo.commit("blue", "test", "prod")
	repo.git("tag", "env-1")
	e, step := gitEngine(t, repo.dir, envconfig.ConfigRefSHA)

	run := func(d *descriptor.Descriptor) *Status {
		t.Helper()
		id := deploy(t, e, ctx, d)
		st, err := e.Status(ctx, id)
		if err != nil || st.State != "SUCCESS" {
			t.Fatalf("%s: %+v %v", id, st, err)
		}
		return st
	}
	st := run(appVersion("app", "prod", "1.0.0", "git-1"))
	if st.ConfigRevision != blue || step.deployOf("git-1").Params["color"] != "blue" {
		t.Fatalf("default ref: revision %s, params %v", st.ConfigRevision, step.deployOf("git-1").Params)
	}
	res, err := e.Wait(ctx, "git-1")
	if err != nil {
		t.Fatal(err)
	}
	if c := res.Steps[0]; c.Name != "config" || c.Output["revision"] != blue || c.Output["ref"] != "refs/heads/main" || !strings.HasSuffix(c.Output["source"], "//app/app/prod.yml") {
		t.Fatalf("config step: %+v", c)
	}

	// A new commit on the default ref is read by the next deploy.
	green := repo.commit("green", "test", "prod")
	if st := run(appVersion("app", "prod", "1.1.0", "git-2")); st.ConfigRevision != green || step.deployOf("git-2").Params["color"] != "green" {
		t.Fatalf("new commit: %s %v", st.ConfigRevision, step.deployOf("git-2").Params)
	}

	// config.ref: a tag and a SHA reachable from main (descriptor_policy.config_ref: sha).
	d := appVersion("app", "test", "1.1.0", "git-3")
	d.Config = &descriptor.ConfigSource{Ref: "env-1"}
	if st := run(d); st.ConfigRevision != blue || step.deployOf("git-3").Params["color"] != "blue" {
		t.Fatalf("tag: %s %v", st.ConfigRevision, step.deployOf("git-3").Params)
	}
	// A ref outside allowed_refs is refused at submission, before being enqueued.
	repo.git("checkout", "-q", "-b", "feature")
	repo.commit("red", "prod")
	repo.git("checkout", "-q", "main")
	d = appVersion("app", "prod", "1.1.0", "git-4")
	d.Config = &descriptor.ConfigSource{Ref: "feature"}
	if _, err := e.Submit(ctx, d); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "not allowed (allowed_refs)") {
		t.Fatalf("feature branch: %v", err)
	}

	// The manual rollback goes back to the configuration revision of the deploy it restores,
	// even though main has moved on.
	sub, err := e.Rollback(ctx, RollbackRequest{Application: "app", Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(ctx, sub.ID); st.ConfigRevision != blue || st.Version != "1.0.0" || step.deployOf(sub.ID).Params["color"] != "blue" {
		t.Fatalf("rollback: %+v %v", st, step.deployOf(sub.ID).Params)
	}

	// The promotion reads the default ref of the destination, or the revision it is given.
	sub, err = e.Promote(ctx, PromoteRequest{Application: "app", From: "test", To: "prod", ConfigRef: blue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Status(ctx, sub.ID); st.ConfigRevision != blue {
		t.Fatalf("promotion with config_ref: %+v", st)
	}
}

func TestConfigRefVietatoDallaPolicy(t *testing.T) {
	ctx := context.Background()
	repo := newGitConfigRepo(t)
	sha := repo.commit("blue", "prod")
	for _, c := range []struct {
		policy, ref, want string
	}{
		{envconfig.ConfigRefNone, "refs/heads/main", "cannot be set by the descriptor"},
		{envconfig.ConfigRefRef, sha, "can name refs, not commit SHAs"},
	} {
		e, _ := gitEngine(t, repo.dir, c.policy)
		d := appVersion("app", "prod", "1.0.0", "")
		d.Config = &descriptor.ConfigSource{Ref: c.ref}
		if _, err := e.Submit(ctx, d); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s/%s: %v", c.policy, c.ref, err)
		}
	}
}

func TestConfigRefSenzaProviderGit(t *testing.T) {
	r := envconfig.Resolver{Provider: envProvider{}}
	d := appVersion("app", "prod", "1.0.0", "")
	d.Config = &descriptor.ConfigSource{Ref: "refs/heads/main"}
	if _, _, err := r.Resolve(context.Background(), *d); err == nil || !strings.Contains(err.Error(), "requires the git environment configuration provider") {
		t.Fatalf("config.ref with a non-git provider: %v", err)
	}
	// A pinned revision (rollback of a deploy made with another provider) is ignored.
	d = appVersion("app", "prod", "1.0.0", "")
	d.Deploy.Targets = []descriptor.Target{{Host: "prod-1"}}
	if _, _, err := r.Resolve(envconfig.WithRevision(context.Background(), "abc"), *d); err != nil {
		t.Fatal(err)
	}
}
