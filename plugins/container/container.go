// Package container releases a container image to servers or VMs over SSH, with Docker or
// Podman: it pulls the image pinned to the digest, replaces the application's container and
// waits until it is running (and "healthy", if the image has a HEALTHCHECK).
//
// The artifact is a registry image (source oci://registry/repository:tag): manent resolves its
// digest, which becomes the artifact's sha256, and the plugin always runs
// "repository@sha256:<digest>". Version and image are written in the container labels, from
// which the plugin reads the release in service and goes back to the previous one.
package container

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

// Name is the name the plugin registers with.
const Name = "container"

// Descriptor parameters (deploy.params) read by the plugin.
const (
	ParamRuntime        = "runtime"        // docker (default) or podman
	ParamContainerName  = "container_name" // default: application name
	ParamRunArgs        = "run_args"       // extra "run" arguments (ports, volumes, env-file, network)
	ParamCommand        = "command"        // arguments passed to the container after the image
	ParamSSHKeySecret   = "ssh_key_secret"
	ParamPasswordSecret = "password_secret"
	ParamSudo           = "sudo"                     // "true": runtime commands with sudo -n
	ParamRegistryUser   = "registry_username"        // registry login before the pull
	ParamRegistrySecret = "registry_password_secret" // secret with the registry password
	ParamWaitTimeout    = "wait_timeout"             // startup wait (default 60s)
	ParamStableFor      = "stable_for"               // without HEALTHCHECK: running time without restarts (default 5s)
)

// Labels written on the container.
const (
	LabelApplication = "manent.application"
	LabelEnvironment = "manent.environment"
	LabelVersion     = "manent.version"
	LabelDeployment  = "manent.deployment"
)

// DetailImage is the Release.Details key with the release image.
const DetailImage = "image"

const (
	defaultWaitTimeout = 60 * time.Second
	defaultStableFor   = 5 * time.Second
)

var (
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector and Rollbacker.
type Plugin struct {
	dialer remotecopy.Dialer
	types  []string
	logger *slog.Logger
	// poll is the check interval while waiting for startup.
	poll time.Duration
}

// New creates the plugin with the given dialer and supported artifact types.
func New(dialer remotecopy.Dialer, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{dialer: dialer, types: types, logger: logger, poll: time.Second}
}

// Factory builds the plugin from the application configuration:
//   - known_hosts, insecure_ignore_host_key: as remote-copy
//   - artifact_types: supported types (default ["container"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	knownHosts, _ := cfg["known_hosts"].(string)
	insecure, _ := cfg["insecure_ignore_host_key"].(bool)
	types := []string{"container"}
	if raw, ok := cfg["artifact_types"].([]any); ok {
		types = types[:0]
		for _, t := range raw {
			s, ok := t.(string)
			if !ok {
				return nil, errors.New("artifact_types must contain strings")
			}
			types = append(types, s)
		}
	}
	return New(&remotecopy.SSHDialer{KnownHostsPath: knownHosts, InsecureIgnoreHostKey: insecure}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if r := req.Params[ParamRuntime]; r != "" && r != "docker" && r != "podman" {
		errs = append(errs, fmt.Errorf("invalid %s %q (docker|podman)", ParamRuntime, r))
	}
	if !nameRe.MatchString(containerName(req)) {
		errs = append(errs, fmt.Errorf("invalid %s %q: letters, digits, '_', '.', '-' are allowed", ParamContainerName, containerName(req)))
	}
	if !versionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes a label of the container: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if req.Params[ParamSSHKeySecret] == "" && req.Params[ParamPasswordSecret] == "" {
		errs = append(errs, fmt.Errorf("provide %q or %q", ParamSSHKeySecret, ParamPasswordSecret))
	}
	if (req.Params[ParamRegistryUser] == "") != (req.Params[ParamRegistrySecret] == "") {
		errs = append(errs, fmt.Errorf("%q and %q must be set together", ParamRegistryUser, ParamRegistrySecret))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("the artifact is not a registry image: provide artifact.source oci://registry/repository:tag"))
	}
	for _, k := range []string{ParamWaitTimeout, ParamStableFor} {
		if _, err := duration(req, k, 0); err != nil {
			errs = append(errs, err)
		}
	}
	for _, t := range req.Targets {
		if t.User == "" {
			errs = append(errs, fmt.Errorf("target %s: user is required", t.Host))
		}
	}
	return errors.Join(errs...)
}

func containerName(req *plugin.Request) string {
	if n := req.Params[ParamContainerName]; n != "" {
		return n
	}
	return req.Application
}

func duration(req *plugin.Request, key string, def time.Duration) (time.Duration, error) {
	raw := req.Params[key]
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: expected a positive duration (e.g. 90s)", key, raw)
	}
	return d, nil
}

// runtime returns the runtime command, with sudo if requested.
func runtime(req *plugin.Request) string {
	r := req.Params[ParamRuntime]
	if r == "" {
		r = "docker"
	}
	if req.Params[ParamSudo] == "true" {
		return "sudo -n " + r
	}
	return r
}

// running describes the application container on a target.
type running struct {
	Version string
	Image   string
	Status  string
}

// inspect reads version, image and state of the container; nil if it does not exist.
func inspect(ctx context.Context, sess remotecopy.Session, rt, name string) (*running, error) {
	q := remotecopy.ShellQuote
	format := `{{index .Config.Labels "` + LabelVersion + `"}}|{{.Config.Image}}|{{.State.Status}}`
	out, err := sess.Run(ctx, "if "+rt+" container inspect "+q(name)+" >/dev/null 2>&1; then "+
		rt+" container inspect -f "+q(format)+" "+q(name)+"; fi")
	if err != nil {
		return nil, fmt.Errorf("reading container %s: %w", name, err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return nil, nil
	}
	parts := strings.SplitN(line, "|", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("unreadable state of container %s: %q", name, line)
	}
	return &running{Version: parts[0], Image: parts[1], Status: parts[2]}, nil
}

// Deploy releases the image to every target in sequence; it stops at the first error. If the
// container in service already has the same image and version it is not restarted, so the
// operation is idempotent.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.release(ctx, t, auth, req, image, req.Artifact.Version, false)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

// release brings a target's container to the given image and version.
func (p *Plugin) release(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request, image, version string, rollback bool) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "rollback", rollback)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()

	rt, name, q := runtime(req), containerName(req), remotecopy.ShellQuote
	details := map[string]string{"container": name, DetailImage: image}
	cur, err := inspect(ctx, sess, rt, name)
	if err != nil {
		return details, err
	}
	if cur != nil {
		details["previous_version"], details["previous_image"] = cur.Version, cur.Image
		if cur.Image == image && cur.Version == version && cur.Status == "running" {
			// Already on the requested release (e.g. retry or recovery): not restarted if ready.
			if state, err := p.wait(ctx, sess, rt, name, req); err == nil {
				log.InfoContext(ctx, "container already on the requested release", "container", name, "version", version)
				details["unchanged"], details["state"] = "true", state
				return details, nil
			}
			log.WarnContext(ctx, "container on the requested release but not ready: it is recreated", "container", name)
		}
	}
	if err := p.login(ctx, sess, rt, req, image); err != nil {
		return details, err
	}
	out, err := sess.Run(ctx, rt+" pull "+q(image))
	if err != nil {
		return details, fmt.Errorf("pulling %s: %w", image, err)
	}
	log.InfoContext(ctx, "image pulled", "image", image, "output", lastLine(out))
	run := []string{rt, "run", "-d", "--name", q(name), "--restart", "unless-stopped",
		"--label", q(LabelApplication + "=" + req.Application),
		"--label", q(LabelEnvironment + "=" + req.Environment),
		"--label", q(LabelVersion + "=" + version),
		"--label", q(LabelDeployment + "=" + req.DeploymentID)}
	if a := req.Params[ParamRunArgs]; a != "" {
		run = append(run, a)
	}
	run = append(run, q(image))
	if c := req.Params[ParamCommand]; c != "" {
		run = append(run, c)
	}
	// The previous container is removed only after a successful pull, to reduce downtime.
	if _, err := sess.Run(ctx, rt+" rm -f "+q(name)+" >/dev/null 2>&1; "+strings.Join(run, " ")); err != nil {
		return details, fmt.Errorf("starting container %s: %w", name, err)
	}
	log.InfoContext(ctx, "container started", "container", name, "image", image, "version", version)
	state, err := p.wait(ctx, sess, rt, name, req)
	details["state"] = state
	if err != nil {
		if logs, lerr := sess.Run(ctx, rt+" logs --tail 20 "+q(name)+" 2>&1"); lerr == nil && strings.TrimSpace(logs) != "" {
			log.WarnContext(ctx, "last lines of the container log", "container", name, "logs", strings.TrimSpace(logs))
		}
		return details, err
	}
	log.InfoContext(ctx, "container ready", "container", name, "state", state)
	return details, nil
}

// login logs in to the image registry passing the password on stdin.
func (p *Plugin) login(ctx context.Context, sess remotecopy.Session, rt string, req *plugin.Request, image string) error {
	user := req.Params[ParamRegistryUser]
	if user == "" {
		return nil
	}
	sr, ok := sess.(remotecopy.StdinRunner)
	if !ok {
		return errors.New("the SSH session does not support registry login")
	}
	pw, err := req.Secrets.Secret(ctx, req.Params[ParamRegistrySecret])
	if err != nil {
		return err
	}
	registry, _, _ := strings.Cut(image, "/")
	q := remotecopy.ShellQuote
	if _, err := sr.RunStdin(ctx, rt+" login -u "+q(user)+" --password-stdin "+q(registry), strings.NewReader(pw)); err != nil {
		return fmt.Errorf("registry login %s: %w", registry, err)
	}
	return nil
}

// wait waits until the container is running: with a HEALTHCHECK until it is "healthy",
// otherwise for stable_for without restarts. An exited or "unhealthy" container is an error.
func (p *Plugin) wait(ctx context.Context, sess remotecopy.Session, rt, name string, req *plugin.Request) (string, error) {
	timeout, _ := duration(req, ParamWaitTimeout, defaultWaitTimeout)
	stable, _ := duration(req, ParamStableFor, defaultStableFor)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := remotecopy.ShellQuote
	format := `{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}|{{.RestartCount}}`
	var since time.Time
	restarts, last := "", ""
	for {
		out, err := sess.Run(ctx, rt+" container inspect -f "+q(format)+" "+q(name))
		if err != nil {
			if ctx.Err() != nil {
				return last, fmt.Errorf("container %s not ready within %s (state %s)", name, timeout, last)
			}
			return last, fmt.Errorf("state of container %s: %w", name, err)
		}
		parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		status, health, count := parts[0], parts[1], parts[2]
		last = strings.Trim(status+" "+health, " ")
		switch {
		case status == "exited" || status == "dead":
			return last, fmt.Errorf("container %s has terminated (state %s)", name, status)
		case health == "unhealthy":
			return last, fmt.Errorf("container %s is unhealthy", name)
		case status == "running" && health == "healthy":
			return last, nil
		case status == "running" && health == "":
			if since.IsZero() || count != restarts {
				since, restarts = time.Now(), count
			} else if time.Since(since) >= stable {
				return last, nil
			}
		default:
			since = time.Time{}
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("container %s not ready within %s (state %s)", name, timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// CurrentRelease reads on every target version and image of the container in service. A
// target without a container (e.g. just added) is ignored; different releases across targets
// are an error, because it would be unclear which one to go back to.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	type release struct{ version, image string }
	found := map[release][]string{}
	for _, t := range req.Targets {
		cur, err := p.currentOn(ctx, t, auth, req)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		if cur == nil || cur.Version == "" {
			continue
		}
		r := release{cur.Version, cur.Image}
		found[r] = append(found[r], t.Host)
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for r, hosts := range found {
			return &plugin.Release{Version: r.version, Details: map[string]string{
				DetailImage: r.image, "source": "container labels", "targets": strings.Join(hosts, ","),
			}}, nil
		}
	}
	var desc []string
	for r, hosts := range found {
		desc = append(desc, r.version+" ("+r.image+") su "+strings.Join(hosts, ","))
	}
	slices.Sort(desc)
	return nil, fmt.Errorf("different releases on the targets: %s", strings.Join(desc, "; "))
}

func (p *Plugin) currentOn(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request) (*running, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()
	return inspect(ctx, sess, runtime(req), containerName(req))
}

// Rollback brings every target's container back to the image of the given release (read from
// the labels before the deploy). It continues even if a target fails, to restore as many
// targets as possible, and reports the errors at the end. It is idempotent.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	image := previous.Details[DetailImage]
	if image == "" || !strings.Contains(image, "@sha256:") {
		return nil, fmt.Errorf("release %s without image pinned to the digest: rollback impossible", previous.Version)
	}
	if !versionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("invalid version %q", previous.Version)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.release(ctx, t, auth, req, image, previous.Version, true)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

// DryRun checks on every target, without changing it, that the deploy can run: connection,
// runtime reachable with the user's permissions and release in service. No pull runs. Unlike
// Deploy it checks all targets.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.check(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) check(ctx context.Context, t plugin.Target, auth remotecopy.Auth, req *plugin.Request) (map[string]string, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()
	rt := runtime(req)
	details := map[string]string{"container": containerName(req)}
	out, err := sess.Run(ctx, rt+" version --format '{{.Server.Version}}'")
	if err != nil {
		return details, fmt.Errorf("runtime %s not usable by user %s: %w", rt, t.User, err)
	}
	details["runtime_version"] = strings.TrimSpace(out)
	cur, err := inspect(ctx, sess, rt, containerName(req))
	if err != nil {
		return details, err
	}
	if cur != nil {
		details["current_version"], details["current_image"] = cur.Version, cur.Image
	}
	if image, err := plugin.ImageReference(req.Artifact); err == nil {
		details[DetailImage] = image
	}
	details["pull"] = "skipped"
	p.logger.InfoContext(ctx, "target ready for the deploy", "deployment", req.DeploymentID, "host", t.Host, "dry_run", true)
	return details, nil
}

// resolveAuth reads the SSH credentials from the secrets only at use time.
func (p *Plugin) resolveAuth(ctx context.Context, req *plugin.Request) (remotecopy.Auth, error) {
	var a remotecopy.Auth
	var err error
	if name := req.Params[ParamSSHKeySecret]; name != "" {
		if a.PrivateKey, err = req.Secrets.Secret(ctx, name); err != nil {
			return a, err
		}
	}
	if name := req.Params[ParamPasswordSecret]; name != "" {
		if a.Password, err = req.Secrets.Secret(ctx, name); err != nil {
			return a, err
		}
	}
	return a, nil
}

// lastLine returns the last non-empty line of the output (e.g. the digest printed by the pull).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
