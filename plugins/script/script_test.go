//go:build unix

package script

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
)

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", fmt.Errorf("secret %q not declared", name)
	}
	return v, nil
}

// writeScript writes an executable sh script and returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// newPlugin builds the plugin from a YAML-like configuration, with the log in a buffer.
func newPlugin(t *testing.T, raw map[string]any) (*Plugin, *bytes.Buffer) {
	t.Helper()
	if _, ok := raw["work_dir"]; !ok {
		raw["work_dir"] = filepath.Join(t.TempDir(), "work")
	}
	if _, ok := raw["artifact_types"]; !ok {
		raw["artifact_types"] = []any{"tgz"}
	}
	var buf bytes.Buffer
	d, err := Factory(raw, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Plugin)
	p.getenv = func(k string) string {
		if k == "PATH" {
			return "/usr/bin:/bin"
		}
		return ""
	}
	return p, &buf
}

func request(params map[string]string, hosts ...string) *plugin.Request {
	req := &plugin.Request{
		DeploymentID: "d-1",
		Tenant:       "payments",
		Application:  "shop",
		Environment:  "prod",
		Artifact:     plugin.Artifact{Type: "tgz", Name: "shop-db", Version: "1.4.0", LocalPath: "/tmp/shop.tgz", SHA256: "abc"},
		Params:       params,
		Secrets:      secrets{"db-password": "s3cr3t-value"},
	}
	for _, h := range hosts {
		req.Targets = append(req.Targets, plugin.Target{Host: h, Port: 5432, Labels: map[string]string{"region": "eu-1"}})
	}
	return req
}

func TestDeploy(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("WORKER_ONLY", "leak")
	run := writeScript(t, bin, "deploy.sh", `
set -eu
echo "deploying $MANENT_APPLICATION/$MANENT_ARTIFACT $MANENT_VERSION with $(cat "$MANENT_SECRETS_DIR/db-password")"
echo "to stderr" >&2
{
  echo "summary=$MANENT_ACTION $MANENT_TENANT $MANENT_ENVIRONMENT $MANENT_PARAM_DB_NAME $MANENT_PARAM_MODE"
  echo "targets=$(tr -d ' \n' < "$MANENT_TARGETS")"
  echo "leak=${WORKER_ONLY:-none} ${PROXY:-}"
  echo "tmp=$TMPDIR"
} >> "$MANENT_OUTPUT"
`)
	sum := sha256.Sum256(must(os.ReadFile(run)))
	p, logs := newPlugin(t, map[string]any{
		"env": map[string]any{"PROXY": "http://proxy:3128"},
		"scripts": map[string]any{
			"migrate": map[string]any{
				"timeout": "1m",
				"params": map[string]any{
					"db.name": map[string]any{"pattern": "[a-z_]+", "required": true},
					"mode":    map[string]any{"pattern": "fast|safe", "default": "safe"},
				},
				"secrets": []any{"db-password"},
				"outputs": map[string]any{
					"summary": map[string]any{"pattern": ".+"},
					"targets": map[string]any{"pattern": ".+"},
					"leak":    map[string]any{"pattern": ".*"},
					"tmp":     map[string]any{"pattern": ".+"},
				},
				"run": map[string]any{"command": []any{run}, "sha256": hex.EncodeToString(sum[:])},
			},
		},
	})
	req := request(map[string]string{"script": "migrate", "db.name": "shop"}, "db1", "db2")
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if got := res.Outputs["summary"]; got != "deploy payments prod shop safe" {
		t.Errorf("summary = %q", got)
	}
	if got := res.Outputs["targets"]; !strings.Contains(got, `"host":"db1"`) || !strings.Contains(got, `"host":"db2"`) {
		t.Errorf("targets = %q", got)
	}
	if got := res.Outputs["leak"]; got != "none http://proxy:3128" {
		t.Errorf("the script must not inherit the worker's variables: %q", got)
	}
	if len(res.Targets) != 2 || res.Targets[0].Status != plugin.StatusDeployed {
		t.Errorf("targets: %+v", res.Targets)
	}
	if strings.Contains(logs.String(), "s3cr3t-value") || !strings.Contains(logs.String(), "***") || !strings.Contains(logs.String(), "to stderr") {
		t.Errorf("log:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Dir(res.Outputs["tmp"])); !os.IsNotExist(err) {
		t.Errorf("the run directory must be removed: %v", err)
	}
}

func TestParametri(t *testing.T) {
	bin := t.TempDir()
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{"s": map[string]any{
		"params": map[string]any{"zone": map[string]any{"pattern": "[a-z]+", "required": true}},
		"run":    map[string]any{"command": []any{writeScript(t, bin, "s.sh", "exit 0\n")}},
	}}})
	for params, want := range map[*map[string]string]string{
		{"zone": "a"}:                          "parameter script is required",
		{"script": "nope", "zone": "a"}:        "not in the catalog",
		{"script": "s"}:                        "zone is required",
		{"script": "s", "zone": "A1"}:          "does not match",
		{"script": "s", "zone": "a", "x": "1"}: "x is not declared",
	} {
		req := request(*params, "h")
		if err := p.Validate(req); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", *params, err, want)
		}
		if _, err := p.Deploy(context.Background(), req); !errors.Is(err, plugin.ErrPermanent) {
			t.Errorf("%v: Deploy must fail permanently: %v", *params, err)
		}
	}
}

func TestEsiti(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "child.pid")
	sh := func(name, body string) map[string]any {
		return map[string]any{"command": []any{writeScript(t, bin, name, body)}}
	}
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{
		"retry":   map[string]any{"run": sh("retry.sh", "exit 75\n")},
		"fail":    map[string]any{"run": sh("fail.sh", "exit 3\n")},
		"slow":    map[string]any{"timeout": "1s", "run": sh("slow.sh", "sleep 60 &\necho $! > "+pidFile+"\nwait\n")},
		"missing": map[string]any{"run": map[string]any{"command": []any{"/nonexistent/manent-script.sh"}}},
		"tamper":  map[string]any{"run": map[string]any{"command": []any{writeScript(t, bin, "t.sh", "exit 0\n")}, "sha256": strings.Repeat("0", 64)}},
		"output":  map[string]any{"run": sh("out.sh", "echo other=1 >> \"$MANENT_OUTPUT\"\n")},
		"noexec":  map[string]any{"run": map[string]any{"command": []any{filepath.Join(bin, "plain.txt")}}},
	}})
	if err := os.WriteFile(filepath.Join(bin, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, permanent := range map[string]bool{"retry": false, "fail": true, "slow": false, "missing": true, "tamper": true, "output": true, "noexec": true} {
		start := time.Now()
		_, err := p.Deploy(context.Background(), request(map[string]string{"script": name}, "h"))
		if err == nil || errors.Is(err, plugin.ErrPermanent) != permanent {
			t.Errorf("%s: %v (permanent %v)", name, err, permanent)
		}
		if name == "slow" && time.Since(start) > 20*time.Second {
			t.Errorf("slow: the timeout did not stop the script (%s)", time.Since(start))
		}
	}
	// The background child of the script that timed out is gone too.
	if b, err := os.ReadFile(pidFile); err == nil {
		pid := strings.TrimSpace(string(b))
		time.Sleep(200 * time.Millisecond)
		n, _ := strconv.Atoi(pid)
		if n > 0 && syscall.Kill(n, 0) == nil {
			t.Errorf("process %s survived the timeout", pid)
		}
	}
}

func TestPerTarget(t *testing.T) {
	bin := t.TempDir()
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{"each": map[string]any{
		"per_target": true,
		"outputs":    map[string]any{"seen": map[string]any{"pattern": ".+"}},
		"run": map[string]any{"command": []any{writeScript(t, bin, "each.sh",
			`echo "seen=$MANENT_TARGET:$MANENT_TARGET_PORT:$MANENT_TARGET_LABEL_REGION:$(tr -d ' \n' < "$MANENT_TARGETS" | wc -c | tr -d ' ')" >> "$MANENT_OUTPUT"
[ "$MANENT_TARGET" != bad ] || exit 3
`)}},
	}}})
	req := request(map[string]string{"script": "each"}, "a", "b", "c")
	req.Parallelism = 2
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range []string{"a", "b", "c"} {
		if r := res.Targets[i]; r.Host != h || r.Status != plugin.StatusDeployed || !strings.HasPrefix(r.Details["seen"], h+":5432:eu-1:") {
			t.Errorf("target %d: %+v", i, r)
		}
	}
	if res.Outputs != nil {
		t.Errorf("per_target outputs go in the targets' details: %v", res.Outputs)
	}
	res, err = p.Deploy(context.Background(), request(map[string]string{"script": "each"}, "bad", "after"))
	if !errors.Is(err, plugin.ErrPermanent) || res.Targets[0].Status != plugin.StatusFailed || res.Targets[1].Status != plugin.StatusSkipped {
		t.Errorf("%v %+v", err, res)
	}
}

func TestReleaseERollback(t *testing.T) {
	bin := t.TempDir()
	state := filepath.Join(bin, "state")
	if err := os.WriteFile(state, []byte("1.3.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{
		"app": map[string]any{
			"outputs": map[string]any{"slot": map[string]any{"pattern": "[ab]"}},
			"run":     map[string]any{"command": []any{writeScript(t, bin, "run.sh", `printf %s "$MANENT_VERSION" > "$1"`), state}},
			"current": map[string]any{"command": []any{writeScript(t, bin, "cur.sh", `[ -s "$1" ] && printf 'version=%s\nslot=a\n' "$(cat "$1")" >> "$MANENT_OUTPUT"; exit 0`), state}},
			"rollback": map[string]any{"command": []any{writeScript(t, bin, "rb.sh",
				`[ "$MANENT_PREVIOUS_SLOT" = a ] || exit 3; printf %s "$MANENT_PREVIOUS_VERSION" > "$1"`), state}},
		},
		"plain": map[string]any{"run": map[string]any{"command": []any{writeScript(t, bin, "plain.sh", "exit 0\n")}}},
	}})
	ctx := context.Background()
	req := request(map[string]string{"script": "app"}, "h")
	prev, err := p.CurrentRelease(ctx, req)
	if err != nil || prev == nil || prev.Version != "1.3.0" || prev.Details["slot"] != "a" {
		t.Fatalf("%+v %v", prev, err)
	}
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Rollback(ctx, req, *prev)
	if err != nil || res.Targets[0].Status != plugin.StatusRolledBack || string(must(os.ReadFile(state))) != "1.3.0" {
		t.Fatalf("%+v %v", res, err)
	}
	if ok, _ := p.RollbackSupported(ctx); !ok {
		t.Error("RollbackSupported")
	}
	plain := request(map[string]string{"script": "plain"}, "h")
	if _, err := p.CurrentRelease(ctx, plain); !errors.Is(err, plugin.ErrPermanent) {
		t.Errorf("current without command: %v", err)
	}
	if _, err := p.Rollback(ctx, plain, *prev); !errors.Is(err, plugin.ErrRollbackUnsupported) {
		t.Errorf("rollback without command: %v", err)
	}
	if _, err := p.DryRun(ctx, plain); !errors.Is(err, plugin.ErrDryRunUnsupported) {
		t.Errorf("dry-run without command: %v", err)
	}
	if err := os.WriteFile(state, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rel, err := p.CurrentRelease(ctx, req); err != nil || rel != nil {
		t.Errorf("no release in service: %+v %v", rel, err)
	}
}

func TestCurrentPerTarget(t *testing.T) {
	bin := t.TempDir()
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{"s": map[string]any{
		"per_target": true,
		"run":        map[string]any{"command": []any{writeScript(t, bin, "r.sh", "exit 0\n")}},
		"current": map[string]any{"command": []any{writeScript(t, bin, "c.sh",
			`case "$MANENT_TARGET" in old) echo version=1.0 ;; *) echo version=2.0 ;; esac >> "$MANENT_OUTPUT"`)}},
	}}})
	if rel, err := p.CurrentRelease(context.Background(), request(map[string]string{"script": "s"}, "a", "b")); err != nil || rel.Version != "2.0" {
		t.Errorf("%+v %v", rel, err)
	}
	if _, err := p.CurrentRelease(context.Background(), request(map[string]string{"script": "s"}, "a", "old")); err == nil || !strings.Contains(err.Error(), "different releases") {
		t.Errorf("%v", err)
	}
}

func TestOutputConSegreto(t *testing.T) {
	bin := t.TempDir()
	p, _ := newPlugin(t, map[string]any{"scripts": map[string]any{"s": map[string]any{
		"secrets": []any{"db-password"},
		"outputs": map[string]any{"x": map[string]any{"pattern": ".+"}},
		"run":     map[string]any{"command": []any{writeScript(t, bin, "s.sh", `echo "x=$(cat "$MANENT_SECRETS_DIR/db-password")" >> "$MANENT_OUTPUT"`)}},
	}}})
	_, err := p.Deploy(context.Background(), request(map[string]string{"script": "s"}, "h"))
	if !errors.Is(err, plugin.ErrPermanent) || !strings.Contains(err.Error(), "contains a secret") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("%v", err)
	}
}

func TestConfigurazione(t *testing.T) {
	run := map[string]any{"command": []any{"/opt/s.sh"}}
	for want, raw := range map[string]map[string]any{
		"artifact_types":           {"scripts": map[string]any{"s": map[string]any{"run": run}}},
		"at least one script":      {"artifact_types": []any{"tgz"}},
		"field tiemout not found":  {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"tiemout": "1m", "run": run}}},
		"run: required":            {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{}}},
		"clean absolute path":      {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"run": map[string]any{"command": []any{"s.sh"}}}}},
		"pattern: required":        {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"run": run, "params": map[string]any{"a": map[string]any{}}}}},
		"default: does not match":  {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"run": run, "params": map[string]any{"a": map[string]any{"pattern": "[0-9]+", "default": "x"}}}}},
		"params: invalid name":     {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"run": run, "params": map[string]any{"script": map[string]any{"pattern": "x"}}}}},
		"env.MANENT_X: set by":     {"artifact_types": []any{"tgz"}, "env": map[string]any{"MANENT_X": "1"}, "scripts": map[string]any{"s": map[string]any{"run": run}}},
		"timeout: between":         {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"timeout": "7h", "run": run}}},
		"scripts: invalid name":    {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"Bad Name": map[string]any{"run": run}}},
		"work_dir":                 {"artifact_types": []any{"tgz"}, "work_dir": "rel", "scripts": map[string]any{"s": map[string]any{"run": run}}},
		"sha256: 64 lowercase hex": {"artifact_types": []any{"tgz"}, "scripts": map[string]any{"s": map[string]any{"run": map[string]any{"command": []any{"/opt/s.sh"}, "sha256": "xyz"}}}},
	} {
		if _, err := ParseConfig(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
