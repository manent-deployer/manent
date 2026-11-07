// Package script runs, on the worker, deploy scripts from a catalog written in manent.yml
// (plugins.script.scripts). The environment configuration chooses a script by name (the
// "script" parameter) and gives the parameters the catalog declares: it never names a command.
// A command on the worker could read what the worker holds (the database, the secret
// provider, the artifacts of other deploys), so only the operators who write manent.yml decide
// what runs there.
//
// Every run gets its own directory under work_dir (mode 0700) with the secrets as files, the
// targets as JSON and the outputs file, and an environment made only of PATH, HOME, TZ, LANG,
// the catalog's env and the MANENT_* variables: the worker's variables are not inherited. The
// directory is removed at the end of the run.
package script

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/pluginutil"
)

// Name is the plugin name.
const Name = "script"

// ParamScript is the deploy parameter that chooses the script of the catalog.
const ParamScript = "script"

// Limits of a script run.
const (
	DefaultTimeout = 10 * time.Minute
	MaxTimeout     = 6 * time.Hour
	// ExitTempFail (EX_TEMPFAIL) asks for a retry.
	ExitTempFail = 75
)

// Config is the plugin configuration (plugins.script).
type Config struct {
	// ArtifactTypes are the artifact types the plugin handles; there is no default, because a
	// script can release anything.
	ArtifactTypes []string `yaml:"artifact_types"`
	// WorkDir holds the directories of the runs (default <temp dir>/manent-script).
	WorkDir string `yaml:"work_dir"`
	// Env are fixed variables given to every script (a proxy, a tool's home); never secrets:
	// those go in Script.Secrets.
	Env map[string]string `yaml:"env"`
	// Scripts is the catalog, by name.
	Scripts map[string]*Script `yaml:"scripts"`
}

// Script is a catalog entry.
type Script struct {
	// Timeout is the maximum duration of a run (default 10m, at most 6h).
	Timeout time.Duration `yaml:"timeout"`
	// PerTarget runs the script once per target, with MANENT_TARGET set, up to the deploy's
	// parallelism at the same time; otherwise one run receives all the targets of the batch.
	PerTarget bool `yaml:"per_target"`
	// Params declares the parameters the script accepts, with the pattern of their values.
	Params map[string]Param `yaml:"params"`
	// Secrets are the logical names of the secrets (declared by the descriptor or by the
	// environment configuration) written as files for the script.
	Secrets []string `yaml:"secrets"`
	// Outputs declares the only output keys the script can write, with their patterns.
	Outputs map[string]Output `yaml:"outputs"`
	// Run is the deploy.
	Run *Action `yaml:"run"`
	// DryRun checks the deploy without changing anything; without it the dry-run only validates.
	DryRun *Action `yaml:"dry_run"`
	// Current prints the release in service (version=<v>); it enables the "plugin" rollback
	// locator and the reconciliation.
	Current *Action `yaml:"current"`
	// Rollback brings the targets back to the release in MANENT_PREVIOUS_VERSION.
	Rollback *Action `yaml:"rollback"`

	params  map[string]*regexp.Regexp
	outputs map[string]*regexp.Regexp
}

// Param is a parameter of a script.
type Param struct {
	// Pattern is a regular expression the whole value must match.
	Pattern  string `yaml:"pattern"`
	Required bool   `yaml:"required"`
	Default  string `yaml:"default"`
}

// Output is an output key of a script.
type Output struct {
	// Pattern is a regular expression the whole value must match.
	Pattern string `yaml:"pattern"`
}

// Action is a command: an absolute path and its arguments, run without a shell.
type Action struct {
	Command []string `yaml:"command"`
	// SHA256 of the Command[0] file, checked before every run.
	SHA256 string `yaml:"sha256"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,62}$`)
	secretRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	envRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Plugin runs the scripts of the catalog.
type Plugin struct {
	cfg Config
	log *slog.Logger
	// getenv reads the worker's variables passed on (PATH, HOME, TZ, LANG); replaced in tests.
	getenv func(string) string
}

// Factory builds the plugin from plugins.script.
func Factory(raw map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &Plugin{cfg: cfg, log: logger, getenv: os.Getenv}, nil
}

// ParseConfig reads and validates plugins.script. Unknown keys are an error: a misspelled
// timeout or pattern must not go unnoticed.
func ParseConfig(raw map[string]any) (Config, error) {
	var cfg Config
	b, err := yaml.Marshal(raw)
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("plugins.script: %w", err)
	}
	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	var errs []error
	if len(c.ArtifactTypes) == 0 {
		errs = append(errs, errors.New("plugins.script.artifact_types: at least one artifact type"))
	}
	if c.WorkDir != "" && !filepath.IsAbs(c.WorkDir) {
		errs = append(errs, fmt.Errorf("plugins.script.work_dir: %q is not an absolute path", c.WorkDir))
	}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		switch {
		case !envRe.MatchString(k):
			errs = append(errs, fmt.Errorf("plugins.script.env: invalid variable name %q", k))
		case strings.HasPrefix(k, "MANENT_") || k == "TMPDIR":
			errs = append(errs, fmt.Errorf("plugins.script.env.%s: set by manent", k))
		case strings.ContainsRune(c.Env[k], 0):
			errs = append(errs, fmt.Errorf("plugins.script.env.%s: NUL in the value", k))
		}
	}
	if len(c.Scripts) == 0 {
		errs = append(errs, errors.New("plugins.script.scripts: at least one script"))
	}
	for _, name := range slices.Sorted(maps.Keys(c.Scripts)) {
		s := c.Scripts[name]
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("plugins.script.scripts: invalid name %q", name))
			continue
		}
		if s == nil {
			errs = append(errs, fmt.Errorf("plugins.script.scripts.%s: empty", name))
			continue
		}
		for _, err := range s.validate() {
			errs = append(errs, fmt.Errorf("plugins.script.scripts.%s.%w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Script) validate() []error {
	var errs []error
	switch {
	case s.Timeout == 0:
		s.Timeout = DefaultTimeout
	case s.Timeout < 0 || s.Timeout > MaxTimeout:
		errs = append(errs, fmt.Errorf("timeout: between 1s and %s", MaxTimeout))
	}
	if s.Run == nil {
		errs = append(errs, errors.New("run: required"))
	}
	for key, a := range map[string]*Action{"run": s.Run, "dry_run": s.DryRun, "current": s.Current, "rollback": s.Rollback} {
		if a == nil {
			continue
		}
		switch {
		case len(a.Command) == 0 || !filepath.IsAbs(a.Command[0]) || filepath.Clean(a.Command[0]) != a.Command[0]:
			errs = append(errs, fmt.Errorf("%s.command: the first element must be a clean absolute path", key))
		case a.SHA256 != "" && !sha256Re.MatchString(a.SHA256):
			errs = append(errs, fmt.Errorf("%s.sha256: 64 lowercase hex characters", key))
		}
		for _, arg := range a.Command {
			if strings.ContainsRune(arg, 0) {
				errs = append(errs, fmt.Errorf("%s.command: NUL in an argument", key))
			}
		}
	}
	s.params = map[string]*regexp.Regexp{}
	for _, k := range slices.Sorted(maps.Keys(s.Params)) {
		p := s.Params[k]
		re, err := fullMatch(p.Pattern)
		switch {
		case !keyRe.MatchString(k) || k == ParamScript:
			errs = append(errs, fmt.Errorf("params: invalid name %q", k))
		case err != nil:
			errs = append(errs, fmt.Errorf("params.%s.pattern: %w", k, err))
		case p.Default != "" && !re.MatchString(p.Default):
			errs = append(errs, fmt.Errorf("params.%s.default: does not match the pattern", k))
		default:
			s.params[k] = re
		}
	}
	s.outputs = map[string]*regexp.Regexp{}
	for _, k := range slices.Sorted(maps.Keys(s.Outputs)) {
		re, err := fullMatch(s.Outputs[k].Pattern)
		switch {
		case !keyRe.MatchString(k) || k == outVersion:
			errs = append(errs, fmt.Errorf("outputs: invalid name %q", k))
		case err != nil:
			errs = append(errs, fmt.Errorf("outputs.%s.pattern: %w", k, err))
		default:
			s.outputs[k] = re
		}
	}
	if len(s.Outputs) > plugin.MaxOutputs {
		errs = append(errs, fmt.Errorf("outputs: at most %d", plugin.MaxOutputs))
	}
	seen := map[string]bool{}
	for _, name := range s.Secrets {
		if !secretRe.MatchString(name) || seen[name] {
			errs = append(errs, fmt.Errorf("secrets: invalid or repeated name %q", name))
		}
		seen[name] = true
	}
	return errs
}

// fullMatch compiles a pattern that must match the whole value; it is required.
func fullMatch(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, errors.New("required")
	}
	return regexp.Compile(`^(?:` + p + `)$`)
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool {
	return slices.Contains(p.cfg.ArtifactTypes, artifactType)
}

// ParallelTargets declares that the plugin honors Request.Parallelism: per_target scripts run
// up to that many targets at the same time, the others receive it in MANENT_PARALLELISM.
func (p *Plugin) ParallelTargets() bool { return true }

// RollbackSupported is true if a script of the catalog has a rollback command: the script is
// chosen by the request, so the one without it fails at the checkpoint, before the deploy.
func (p *Plugin) RollbackSupported(context.Context) (bool, error) {
	for _, s := range p.cfg.Scripts {
		if s.Rollback != nil {
			return true, nil
		}
	}
	return false, nil
}

// Validate checks the script and its parameters.
func (p *Plugin) Validate(req *plugin.Request) error {
	_, _, _, err := p.prepare(req)
	return err
}

// Deploy runs the script.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	name, s, params, err := p.prepare(req)
	if err != nil {
		return nil, plugin.Permanent(err)
	}
	return p.call(ctx, req, name, s, actionDeploy, s.Run, params, nil)
}

// DryRun runs the script's dry_run command; without one, the dry-run only validates.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	name, s, params, err := p.prepare(req)
	if err != nil {
		return nil, plugin.Permanent(err)
	}
	if s.DryRun == nil {
		return nil, plugin.ErrDryRunUnsupported
	}
	return p.call(ctx, req, name, s, actionDryRun, s.DryRun, params, nil)
}

// Rollback runs the script's rollback command with the previous release.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	name, s, params, err := p.prepare(req)
	if err != nil {
		return nil, plugin.Permanent(err)
	}
	if s.Rollback == nil {
		return nil, plugin.Permanent(fmt.Errorf("script %s has no rollback command: %w", name, plugin.ErrRollbackUnsupported))
	}
	extra := map[string]string{"MANENT_PREVIOUS_VERSION": previous.Version}
	for k, v := range previous.Details {
		extra["MANENT_PREVIOUS_"+envName(k)] = v
	}
	return p.call(ctx, req, name, s, actionRollback, s.Rollback, params, extra)
}

// CurrentRelease runs the script's current command: the version= output is the release in
// service, none means no release. With per_target the targets must agree.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	name, s, params, err := p.prepare(req)
	if err != nil {
		return nil, plugin.Permanent(err)
	}
	if s.Current == nil {
		return nil, plugin.Permanent(fmt.Errorf("script %s has no current command: it cannot read the release in service", name))
	}
	c := p.newCall(req, name, s, actionCurrent, s.Current, params, nil)
	if !s.PerTarget {
		out, err := c.exec(ctx, nil)
		if err != nil {
			return nil, err
		}
		return release(out), nil
	}
	var rel *plugin.Release
	res, err := plugin.ForEachTarget(ctx, req, func(ctx context.Context, t plugin.Target) (plugin.TargetResult, error) {
		out, err := c.exec(ctx, &t)
		if err != nil {
			return plugin.TargetResult{}, fmt.Errorf("target %s: %w", t.Host, err)
		}
		return plugin.TargetResult{Status: plugin.StatusReady, Details: out}, nil
	})
	if err != nil {
		return nil, err
	}
	for i, r := range res {
		got := release(r.Details)
		switch {
		case i == 0:
			rel = got
		case (rel == nil) != (got == nil) || (rel != nil && rel.Version != got.Version):
			return nil, fmt.Errorf("the targets have different releases in service (%s: %s, %s: %s)",
				res[0].Host, versionOf(rel), r.Host, versionOf(got))
		}
	}
	return rel, nil
}

// release turns the outputs of the current command into a release.
func release(out map[string]string) *plugin.Release {
	v := out[outVersion]
	if v == "" {
		return nil
	}
	details := maps.Clone(out)
	delete(details, outVersion)
	if len(details) == 0 {
		details = nil
	}
	return &plugin.Release{Version: v, Details: details}
}

func versionOf(r *plugin.Release) string {
	if r == nil {
		return "none"
	}
	return r.Version
}

// prepare finds the script of the request and the values of its parameters, defaults
// included.
func (p *Plugin) prepare(req *plugin.Request) (string, *Script, map[string]string, error) {
	name := req.Params[ParamScript]
	if name == "" {
		return "", nil, nil, fmt.Errorf("parameter %s is required: the name of a script in plugins.script.scripts", ParamScript)
	}
	s, ok := p.cfg.Scripts[name]
	if !ok {
		return "", nil, nil, fmt.Errorf("script %q is not in the catalog (plugins.script.scripts)", name)
	}
	var errs []error
	params := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(req.Params)) {
		if k == ParamScript {
			continue
		}
		re, ok := s.params[k]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("parameter %s is not declared by script %s", k, name))
		case !re.MatchString(req.Params[k]):
			errs = append(errs, fmt.Errorf("parameter %s: the value does not match the pattern of script %s", k, name))
		default:
			params[k] = req.Params[k]
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Params)) {
		if _, set := params[k]; set {
			continue
		}
		switch d := s.Params[k]; {
		case d.Default != "":
			params[k] = d.Default
		case d.Required:
			errs = append(errs, fmt.Errorf("parameter %s is required by script %s", k, name))
		}
	}
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) && req.Artifact.Version != "" {
		errs = append(errs, fmt.Errorf("invalid artifact version %q", req.Artifact.Version))
	}
	return name, s, params, errors.Join(errs...)
}
