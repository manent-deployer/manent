package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/manent-deployer/manent/internal/cmdout"
	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/pluginutil"
)

// Actions, in MANENT_ACTION.
const (
	actionDeploy   = "deploy"
	actionDryRun   = "dry-run"
	actionCurrent  = "current"
	actionRollback = "rollback"
)

// outVersion is the output of the current command with the release in service.
const outVersion = "version"

const (
	// maxLogBytes is the output of a run written to the deploy log.
	maxLogBytes = 256 << 10
	// staleAfter is the age of a run directory left by a crash, removed by the next runs: well
	// beyond MaxTimeout, so a running script never loses its directory.
	staleAfter = 24 * time.Hour
	// killDelay is how long the process group has to exit after SIGTERM.
	killDelay = 10 * time.Second
)

// inheritedEnv are the only worker variables a script receives.
var inheritedEnv = []string{"PATH", "HOME", "TZ", "LANG"}

// call is a command of a script for a request.
type call struct {
	p      *Plugin
	req    *plugin.Request
	name   string
	s      *Script
	action string
	a      *Action
	params map[string]string
	extra  map[string]string
}

func (p *Plugin) newCall(req *plugin.Request, name string, s *Script, action string, a *Action, params, extra map[string]string) *call {
	return &call{p: p, req: req, name: name, s: s, action: action, a: a, params: params, extra: extra}
}

// call runs the command once, or once per target with per_target, and builds the result.
func (p *Plugin) call(ctx context.Context, req *plugin.Request, name string, s *Script, action string, a *Action,
	params, extra map[string]string) (*plugin.Result, error) {
	c := p.newCall(req, name, s, action, a, params, extra)
	ok := map[string]string{actionDeploy: plugin.StatusDeployed, actionDryRun: plugin.StatusReady, actionRollback: plugin.StatusRolledBack}[action]
	res := &plugin.Result{Plugin: Name}
	if s.PerTarget {
		run := plugin.ForEachTarget
		if action != actionDeploy {
			// A rollback must reach every target, a dry-run report on all of them.
			run = plugin.ForAllTargets
		}
		var err error
		res.Targets, err = run(ctx, req, func(ctx context.Context, t plugin.Target) (plugin.TargetResult, error) {
			out, err := c.exec(ctx, &t)
			if err != nil {
				return plugin.TargetResult{}, fmt.Errorf("target %s: %w", t.Host, err)
			}
			return plugin.TargetResult{Status: ok, Details: out}, nil
		})
		return res, err
	}
	out, err := c.exec(ctx, nil)
	status := ok
	if err != nil {
		status = plugin.StatusFailed
	}
	for _, t := range req.Targets {
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: status})
	}
	if action == actionDeploy {
		res.Outputs = out
	}
	return res, err
}

// exec runs the command for the request, or for a single target. It returns the outputs the
// script wrote, checked against the declared ones.
func (c *call) exec(ctx context.Context, target *plugin.Target) (map[string]string, error) {
	if err := c.checkBinary(); err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	for _, name := range c.s.Secrets {
		v, err := c.req.Secrets.Secret(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		secrets[name] = v
	}
	red := cmdout.NewRedactor(slices.Collect(maps.Values(secrets)))

	dir, err := c.p.runDir(c.req.DeploymentID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			c.p.log.WarnContext(ctx, "removing the run directory failed", "dir", dir, "error", err)
		}
	}()
	if err := c.writeFiles(dir, secrets, target); err != nil {
		return nil, err
	}

	rctx, cancel := context.WithTimeout(ctx, c.s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, c.a.Command[0], c.a.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = c.env(dir, target)
	cmd.Stdin = nil
	attrs := []any{"script", c.name, "action", c.action}
	if target != nil {
		attrs = append(attrs, "target", target.Host)
	}
	budget := cmdout.NewBudget(maxLogBytes)
	stdout := &cmdout.LineWriter{Ctx: ctx, Log: c.p.log, Red: red, Budget: budget, Attrs: append(slices.Clip(attrs), "stream", "stdout")}
	stderr := &cmdout.LineWriter{Ctx: ctx, Log: c.p.log, Red: red, Budget: budget, Attrs: append(slices.Clip(attrs), "stream", "stderr")}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// The script's children end with it: a timeout must not leave a deploy running.
	processGroup(cmd)
	cmd.WaitDelay = killDelay

	c.p.log.InfoContext(ctx, "script started", attrs...)
	start := time.Now()
	err = cmd.Run()
	endGroup(cmd)
	stdout.Flush()
	stderr.Flush()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("script %s interrupted: %w", c.name, ctx.Err())
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("script %s terminated by timeout after %s", c.name, c.s.Timeout)
	case errors.As(err, &exit) && exit.ExitCode() == ExitTempFail:
		return nil, fmt.Errorf("script %s asked for a retry (exit %d)", c.name, ExitTempFail)
	case errors.As(err, &exit) && exit.ExitCode() < 0:
		return nil, fmt.Errorf("script %s killed by a signal", c.name)
	case errors.As(err, &exit):
		return nil, plugin.Permanent(fmt.Errorf("script %s exited with code %d", c.name, exit.ExitCode()))
	case errors.Is(err, exec.ErrWaitDelay):
		// The script ended well, but a child kept its output open: it was killed.
		c.p.log.WarnContext(ctx, "a process started by the script kept its output open and was stopped", attrs...)
	case err != nil:
		// The command cannot start: not executable, a missing interpreter.
		return nil, plugin.Permanent(fmt.Errorf("script %s: %s", c.name, red.Apply(err.Error())))
	}
	c.p.log.InfoContext(ctx, "script completed", append(attrs, "duration_ms", time.Since(start).Milliseconds())...)
	return c.outputs(filepath.Join(dir, "output"), red)
}

// checkBinary checks that the command exists and, with sha256, that it is the expected file.
// Both are permanent: the file does not change between retries on the same worker.
func (c *call) checkBinary() error {
	f, err := os.Open(c.a.Command[0])
	if err != nil {
		return plugin.Permanent(fmt.Errorf("script %s: %w (is the script installed on this worker?)", c.name, err))
	}
	defer f.Close()
	if c.a.SHA256 == "" {
		return nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("script %s: reading %s: %w", c.name, c.a.Command[0], err)
	}
	if hex.EncodeToString(h.Sum(nil)) != c.a.SHA256 {
		return plugin.Permanent(fmt.Errorf("script %s: the sha256 of %s does not match the catalog", c.name, c.a.Command[0]))
	}
	return nil
}

// runDir creates the directory of a run (0700) and removes the ones left by crashes.
func (p *Plugin) runDir(deploymentID string) (string, error) {
	base := p.cfg.WorkDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "manent-script")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("work_dir: %w", err)
	}
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && e.IsDir() && time.Since(info.ModTime()) > staleAfter {
				_ = os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
	dir, err := os.MkdirTemp(base, safeName(deploymentID)+"-")
	if err != nil {
		return "", fmt.Errorf("work_dir: %w", err)
	}
	return dir, nil
}

// writeFiles writes secrets, targets and the empty outputs file.
func (c *call) writeFiles(dir string, secrets map[string]string, target *plugin.Target) error {
	for _, sub := range []string{"secrets", "tmp"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	targets := c.req.Targets
	if target != nil {
		targets = []plugin.Target{*target}
	}
	if targets == nil {
		targets = []plugin.Target{}
	}
	tj, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	files := map[string][]byte{"targets.json": tj, "output": nil}
	for name, v := range secrets {
		files[filepath.Join("secrets", name)] = []byte(v)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// env is the whole environment of the script.
func (c *call) env(dir string, target *plugin.Target) []string {
	r := c.req
	vars := map[string]string{}
	for _, k := range inheritedEnv {
		if v := c.p.getenv(k); v != "" {
			vars[k] = v
		}
	}
	maps.Copy(vars, c.p.cfg.Env)
	maps.Copy(vars, map[string]string{
		"TMPDIR":                 filepath.Join(dir, "tmp"),
		"MANENT_ACTION":          c.action,
		"MANENT_DRY_RUN":         strconv.FormatBool(c.action == actionDryRun),
		"MANENT_SCRIPT":          c.name,
		"MANENT_DEPLOYMENT_ID":   r.DeploymentID,
		"MANENT_TENANT":          r.Tenant,
		"MANENT_APPLICATION":     r.Application,
		"MANENT_ARTIFACT":        r.Artifact.Name,
		"MANENT_ENVIRONMENT":     r.Environment,
		"MANENT_VERSION":         r.Artifact.Version,
		"MANENT_ARTIFACT_TYPE":   r.Artifact.Type,
		"MANENT_ARTIFACT_PATH":   r.Artifact.LocalPath,
		"MANENT_ARTIFACT_SHA256": r.Artifact.SHA256,
		"MANENT_PARALLELISM":     strconv.Itoa(max(1, r.Parallelism)),
		"MANENT_TARGETS":         filepath.Join(dir, "targets.json"),
		"MANENT_SECRETS_DIR":     filepath.Join(dir, "secrets"),
		"MANENT_OUTPUT":          filepath.Join(dir, "output"),
		"MANENT_WORK_DIR":        dir,
	})
	if target != nil {
		vars["MANENT_TARGET"] = target.Host
		if target.Port != 0 {
			vars["MANENT_TARGET_PORT"] = strconv.Itoa(target.Port)
		}
		if target.User != "" {
			vars["MANENT_TARGET_USER"] = target.User
		}
		for k, v := range target.Labels {
			vars["MANENT_TARGET_LABEL_"+envName(k)] = v
		}
	}
	for k, v := range c.params {
		vars["MANENT_PARAM_"+envName(k)] = v
	}
	maps.Copy(vars, c.extra)
	env := make([]string, 0, len(vars))
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if v := vars[k]; v != "" && !strings.ContainsRune(v, 0) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// outputs reads the outputs file and keeps only the declared keys with valid values; for the
// current command also version.
func (c *call) outputs(path string, red *cmdout.Redactor) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the outputs: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, plugin.MaxOutputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the outputs: %w", err)
	}
	if len(b) > plugin.MaxOutputBytes {
		return nil, plugin.Permanent(fmt.Errorf("script %s: outputs over %d bytes", c.name, plugin.MaxOutputBytes))
	}
	out, err := cmdout.ParseOutputs(string(b))
	if err != nil {
		return nil, plugin.Permanent(fmt.Errorf("script %s: %w", c.name, err))
	}
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(out)) {
		var re *regexp.Regexp
		if k == outVersion && c.action == actionCurrent {
			re = pluginutil.VersionRe
		} else {
			re = c.s.outputs[k]
		}
		switch {
		case re == nil:
			errs = append(errs, fmt.Errorf("output %s is not declared by the script", k))
		case !re.MatchString(out[k]):
			errs = append(errs, fmt.Errorf("output %s: the value does not match the pattern", k))
		case red.Contains(out[k]):
			// Outputs are persisted by DBOS.
			errs = append(errs, fmt.Errorf("output %s contains a secret", k))
		}
	}
	if len(errs) > 0 {
		return nil, plugin.Permanent(fmt.Errorf("script %s: %w", c.name, errors.Join(errs...)))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

var envUnsafe = regexp.MustCompile(`[^A-Z0-9]+`)

// envName turns a name into part of a variable name (db.name → DB_NAME).
func envName(s string) string { return envUnsafe.ReplaceAllString(strings.ToUpper(s), "_") }

// safeName makes an ID usable as a file name.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == '*' {
			return '_'
		}
		return r
	}, strings.TrimLeft(s, "."))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
