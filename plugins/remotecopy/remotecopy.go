// Package remotecopy is the first deploy plugin: it copies the artifact to remote servers over
// SSH/SFTP, verifies its integrity and, optionally, runs a post-copy command.
//
// With the "releases" layout every version has its own directory
// (<dest_dir>/releases/<version>) and the <dest_dir>/current symlink marks the one in service:
// the plugin can therefore read the current release and go back to the previous one (rollback).
package remotecopy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/joeg-ita/manent/pkg/plugin"
)

// Name is the name the plugin registers with.
const Name = "remote-copy"

// Descriptor parameters (deploy.params) read by the plugin.
const (
	ParamDestDir        = "dest_dir"        // required: remote destination directory
	ParamPostCommand    = "post_command"    // optional: command run after the copy
	ParamSSHKeySecret   = "ssh_key_secret"  // name of the secret with the PEM private key
	ParamPasswordSecret = "password_secret" // alternatively: name of the secret with the password
	ParamVerify         = "verify_checksum" // "false" to disable the remote sha256 check
	// ParamLayout is "flat" (default: file in dest_dir) or "releases" (needed for rollback).
	ParamLayout = "layout"
	// ParamKeepReleases is the number of releases kept with the releases layout (default 5, at least 2).
	ParamKeepReleases = "keep_releases"
	// ParamPrepareCommand, releases layout only, runs before the switch (e.g. extraction).
	ParamPrepareCommand = "prepare_command"
	// ParamRollbackCommand runs after the rollback on every target (default: post_command).
	ParamRollbackCommand = "rollback_command"
)

// Layouts of the destination directory.
const (
	LayoutFlat     = "flat"
	LayoutReleases = "releases"
)

// defaultKeepReleases is the number of releases kept when keep_releases is not set.
const defaultKeepReleases = 5

// releaseRe limits versions, which with the releases layout become remote directory names.
var releaseRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// Auth holds the credentials resolved at connection time.
type Auth struct {
	PrivateKey string
	Password   string
}

// Session is a connection to a single remote host.
type Session interface {
	Upload(ctx context.Context, localPath, remotePath string) error
	Run(ctx context.Context, cmd string) (string, error)
	Close() error
}

// StdinRunner is implemented by sessions that can pass data on a command's stdin (e.g. a
// password), without it showing up in the remote command line.
type StdinRunner interface {
	RunStdin(ctx context.Context, cmd string, stdin io.Reader) (string, error)
}

// Dialer opens sessions to remote hosts; abstracted so tests can replace it.
type Dialer interface {
	Dial(ctx context.Context, target plugin.Target, auth Auth) (Session, error)
}

// Plugin implements plugin.Deployer.
type Plugin struct {
	dialer Dialer
	types  []string
	logger *slog.Logger
}

// New creates the plugin with the given dialer and supported artifact types.
func New(dialer Dialer, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{dialer: dialer, types: types, logger: logger}
}

// Factory builds the plugin from the application configuration:
//   - known_hosts: path of the known_hosts file (default ~/.ssh/known_hosts)
//   - insecure_ignore_host_key: true to skip host key verification (development only)
//   - artifact_types: list of supported types (default ["application"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	knownHosts, _ := cfg["known_hosts"].(string)
	insecure, _ := cfg["insecure_ignore_host_key"].(bool)
	types := []string{"application"}
	if raw, ok := cfg["artifact_types"].([]any); ok {
		types = types[:0]
		for _, t := range raw {
			s, ok := t.(string)
			if !ok {
				return nil, fmt.Errorf("artifact_types must contain strings")
			}
			types = append(types, s)
		}
	}
	return New(&SSHDialer{KnownHostsPath: knownHosts, InsecureIgnoreHostKey: insecure}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool {
	for _, t := range p.types {
		if t == artifactType {
			return true
		}
	}
	return false
}

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if req.Params[ParamDestDir] == "" {
		errs = append(errs, fmt.Errorf("parameter %q is required", ParamDestDir))
	}
	if req.Params[ParamSSHKeySecret] == "" && req.Params[ParamPasswordSecret] == "" {
		errs = append(errs, fmt.Errorf("provide %q or %q", ParamSSHKeySecret, ParamPasswordSecret))
	}
	if req.Artifact.LocalPath == "" {
		errs = append(errs, errors.New("local artifact missing (artifacts step not run?)"))
	}
	for _, t := range req.Targets {
		if t.User == "" {
			errs = append(errs, fmt.Errorf("target %s: user is required", t.Host))
		}
	}
	switch layout(req) {
	case LayoutFlat:
		if req.Params[ParamPrepareCommand] != "" {
			errs = append(errs, fmt.Errorf("%q requires layout %q", ParamPrepareCommand, LayoutReleases))
		}
	case LayoutReleases:
		if !releaseRe.MatchString(req.Artifact.Version) {
			errs = append(errs, fmt.Errorf("with layout %q artifact.version is required and becomes the directory name: letters, digits, '.', '_', '+', '-' are allowed", LayoutReleases))
		}
		if _, err := keepReleases(req); err != nil {
			errs = append(errs, err)
		}
	default:
		errs = append(errs, fmt.Errorf("invalid %s %q (%s|%s)", ParamLayout, req.Params[ParamLayout], LayoutFlat, LayoutReleases))
	}
	return errors.Join(errs...)
}

// layout returns the requested layout.
func layout(req *plugin.Request) string {
	if l := req.Params[ParamLayout]; l != "" {
		return l
	}
	return LayoutFlat
}

// keepReleases reads keep_releases: at least 2, to always keep the previous release.
func keepReleases(req *plugin.Request) (int, error) {
	raw := req.Params[ParamKeepReleases]
	if raw == "" {
		return defaultKeepReleases, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2 {
		return 0, fmt.Errorf("%s must be an integer >= 2", ParamKeepReleases)
	}
	return n, nil
}

// paths computes the remote paths of the artifact and, with the releases layout, of the release.
func paths(req *plugin.Request) (remote, releaseDir string) {
	dest := req.Params[ParamDestDir]
	if layout(req) == LayoutReleases {
		releaseDir = path.Join(dest, LayoutReleases, req.Artifact.Version)
		return path.Join(releaseDir, path.Base(req.Artifact.LocalPath)), releaseDir
	}
	return path.Join(dest, path.Base(req.Artifact.LocalPath)), ""
}

// Deploy copies the artifact to every target in sequence; it stops at the first error.
// The copy overwrites the remote file, so the operation is idempotent.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()

	dest := req.Params[ParamDestDir]
	remote, releaseDir := paths(req)
	details := map[string]string{"remote_path": remote}
	var previous string
	if releaseDir != "" {
		// The current release, read before the switch, is never deleted by the cleanup.
		if previous, err = readCurrent(ctx, sess, dest); err != nil {
			return nil, err
		}
	}
	if _, err := sess.Run(ctx, "mkdir -p "+shellQuote(path.Dir(remote))); err != nil {
		return nil, fmt.Errorf("creating the remote directory: %w", err)
	}
	if err := sess.Upload(ctx, req.Artifact.LocalPath, remote); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	log.InfoContext(ctx, "artefatto copiato", "remote_path", remote)

	if req.Params[ParamVerify] != "false" && req.Artifact.SHA256 != "" {
		out, err := sess.Run(ctx, "sha256sum "+shellQuote(remote))
		if err != nil {
			return nil, fmt.Errorf("checksum verification: %w", err)
		}
		got, _, _ := strings.Cut(strings.TrimSpace(out), " ")
		if !strings.EqualFold(got, req.Artifact.SHA256) {
			return nil, fmt.Errorf("remote checksum %s differs from %s", got, req.Artifact.SHA256)
		}
		details["checksum"] = "verified"
	}
	env := ""
	if releaseDir != "" {
		env = commandEnv(dest, releaseDir, remote, req.Artifact.Version)
		if cmd := req.Params[ParamPrepareCommand]; cmd != "" {
			if err := p.runCommand(ctx, log, sess, "prepare_command", env+cmd); err != nil {
				return nil, err
			}
			details["prepare_command"] = "ok"
		}
		if err := switchCurrent(ctx, sess, dest, req.Artifact.Version); err != nil {
			return nil, err
		}
		details["release"], details["previous"] = req.Artifact.Version, previous
		log.InfoContext(ctx, "release attivata", "release", req.Artifact.Version, "previous", previous)
	}
	if cmd := req.Params[ParamPostCommand]; cmd != "" {
		if err := p.runCommand(ctx, log, sess, "post_command", env+cmd); err != nil {
			return nil, err
		}
		details["post_command"] = "ok"
	}
	if releaseDir != "" {
		keep, _ := keepReleases(req)
		if _, err := sess.Run(ctx, pruneCommand(dest, keep, req.Artifact.Version, previous)); err != nil {
			// Cleanup does not compromise the deploy: extra releases stay on the target.
			log.WarnContext(ctx, "pulizia delle release precedenti fallita", "error", err)
		}
	}
	return details, nil
}

// runCommand runs a configured command and logs its output.
func (p *Plugin) runCommand(ctx context.Context, log *slog.Logger, sess Session, name, cmd string) error {
	out, err := sess.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	log.InfoContext(ctx, name+" eseguito", "output", strings.TrimSpace(out))
	return nil
}

// currentLink is the symlink to the release in service, relative to dest_dir.
const currentLink = "current"

// readCurrent returns the version the current symlink points to, or "" if it does not exist.
func readCurrent(ctx context.Context, sess Session, dest string) (string, error) {
	link := shellQuote(path.Join(dest, currentLink))
	out, err := sess.Run(ctx, "if [ -L "+link+" ]; then readlink "+link+"; fi")
	if err != nil {
		return "", fmt.Errorf("reading the current release: %w", err)
	}
	if v := strings.TrimSpace(out); v != "" {
		return path.Base(v), nil
	}
	return "", nil
}

// switchCurrent points current to the given release with an atomic symlink swap (rename of a
// temporary symlink; requires GNU coreutils "mv -T").
func switchCurrent(ctx context.Context, sess Session, dest, version string) error {
	tmp := shellQuote(path.Join(dest, ".current.tmp"))
	cmd := "ln -sfn " + shellQuote(path.Join(LayoutReleases, version)) + " " + tmp +
		" && mv -Tf " + tmp + " " + shellQuote(path.Join(dest, currentLink))
	if _, err := sess.Run(ctx, cmd); err != nil {
		return fmt.Errorf("switching release %s: %w", version, err)
	}
	return nil
}

// pruneCommand deletes the oldest releases (by modification time) beyond keep, never touching
// the one just activated and the previous one.
func pruneCommand(dest string, keep int, current, previous string) string {
	exclude := []string{current}
	if previous != "" && previous != current {
		exclude = append(exclude, previous)
	}
	grep := "grep -vxF"
	for _, e := range exclude {
		grep += " -e " + shellQuote(e)
	}
	return "cd " + shellQuote(path.Join(dest, LayoutReleases)) + " && ls -1t | " + grep +
		" | tail -n +" + strconv.Itoa(keep-len(exclude)+1) +
		` | while IFS= read -r r; do rm -rf -- "$r"; done`
}

// commandEnv returns the assignments of the variables passed to the configured commands.
// MANENT_ARTIFACT is empty in rollbacks, when the release file name is unknown.
func commandEnv(dest, releaseDir, artifact, version string) string {
	vars := []string{
		"MANENT_RELEASE_DIR=" + shellQuote(releaseDir),
		"MANENT_ARTIFACT=" + shellQuote(artifact),
		"MANENT_VERSION=" + shellQuote(version),
		"MANENT_CURRENT=" + shellQuote(path.Join(dest, currentLink)),
	}
	return strings.Join(vars, " ") + "; export MANENT_RELEASE_DIR MANENT_ARTIFACT MANENT_VERSION MANENT_CURRENT; "
}

// CurrentRelease reads on every target the release current points to (releases layout).
// A target without current (e.g. just added) is ignored; different versions across targets
// are an error, because it would be unclear which release to go back to.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	if layout(req) != LayoutReleases {
		return nil, fmt.Errorf("the release in service is read only with %s %q", ParamLayout, LayoutReleases)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for _, t := range req.Targets {
		v, err := p.currentOn(ctx, t, auth, req.Params[ParamDestDir])
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		if v != "" {
			found[v] = append(found[v], t.Host)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v, hosts := range found {
			return &plugin.Release{Version: v, Details: map[string]string{"source": "symlink " + currentLink, "targets": strings.Join(hosts, ",")}}, nil
		}
	}
	var desc []string
	for _, v := range slices.Sorted(maps.Keys(found)) {
		desc = append(desc, v+" su "+strings.Join(found[v], ","))
	}
	return nil, fmt.Errorf("different releases on the targets: %s", strings.Join(desc, "; "))
}

func (p *Plugin) currentOn(ctx context.Context, t plugin.Target, auth Auth, dest string) (string, error) {
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return "", fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()
	return readCurrent(ctx, sess, dest)
}

// Rollback moves current back to the given release on all targets and runs rollback_command
// (or, if absent, post_command). It continues even if a target fails, to restore as many
// targets as possible, and reports the errors at the end. It is idempotent.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if layout(req) != LayoutReleases {
		return nil, fmt.Errorf("rollback requires %s %q", ParamLayout, LayoutReleases)
	}
	if !releaseRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("version %q is not valid as a release name", previous.Version)
	}
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.rollbackTarget(ctx, t, auth, req, previous.Version)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request, version string) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "rollback", true)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()

	dest := req.Params[ParamDestDir]
	releaseDir := path.Join(dest, LayoutReleases, version)
	if _, err := sess.Run(ctx, "test -d "+shellQuote(releaseDir)); err != nil {
		return nil, fmt.Errorf("release %s missing on the target: %w", version, err)
	}
	if err := switchCurrent(ctx, sess, dest, version); err != nil {
		return nil, err
	}
	details := map[string]string{"release": version}
	log.WarnContext(ctx, "release precedente ripristinata", "release", version)
	cmd := req.Params[ParamRollbackCommand]
	if cmd == "" {
		cmd = req.Params[ParamPostCommand]
	}
	if cmd != "" {
		// The variables point to the restored release.
		env := commandEnv(dest, releaseDir, "", version)
		if err := p.runCommand(ctx, log, sess, "rollback_command", env+cmd); err != nil {
			return details, err
		}
		details["rollback_command"] = "ok"
	}
	return details, nil
}

// DryRun checks on every target, without changing it, that the deploy can run: connection and
// credentials, writable (or creatable) destination directory and, if needed, sha256sum.
// post_command does not run. Unlike Deploy it checks all targets.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	auth, err := p.resolveAuth(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, auth, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, auth Auth, req *plugin.Request) (map[string]string, error) {
	log := p.logger.With("deployment", req.DeploymentID, "host", t.Host, "dry_run", true)
	sess, err := p.dialer.Dial(ctx, t, auth)
	if err != nil {
		return nil, fmt.Errorf("connection: %w", err)
	}
	defer sess.Close()

	remote, releaseDir := paths(req)
	details := map[string]string{"remote_path": remote}
	if _, err := sess.Run(ctx, writableDirCheck(req.Params[ParamDestDir])); err != nil {
		return details, fmt.Errorf("remote directory not writable: %w", err)
	}
	details["dest_dir"] = "writable"
	if releaseDir != "" {
		cur, err := readCurrent(ctx, sess, req.Params[ParamDestDir])
		if err != nil {
			return details, err
		}
		details["current"] = cur
		if req.Params[ParamPrepareCommand] != "" {
			details["prepare_command"] = "skipped"
		}
	}
	if req.Params[ParamVerify] != "false" && req.Artifact.SHA256 != "" {
		if _, err := sess.Run(ctx, "command -v sha256sum"); err != nil {
			return details, fmt.Errorf("sha256sum not available: %w", err)
		}
		details["checksum"] = "available"
	}
	if req.Params[ParamPostCommand] != "" {
		details["post_command"] = "skipped"
	}
	log.InfoContext(ctx, "target pronto per il deploy", "remote_path", remote)
	return details, nil
}

// writableDirCheck returns a command that, without creating anything, climbs to the first
// existing directory of the path (the one where "mkdir -p" would create the missing ones) and
// checks it is writable.
func writableDirCheck(dir string) string {
	return "d=" + shellQuote(dir) + `; while [ ! -e "$d" ]; do d=$(dirname "$d"); done; test -d "$d" && test -w "$d"`
}

// resolveAuth reads the credentials from the secrets only at deploy time.
func (p *Plugin) resolveAuth(ctx context.Context, req *plugin.Request) (Auth, error) {
	var a Auth
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

// ShellQuote wraps an argument in single quotes for the remote shell.
func ShellQuote(s string) string { return shellQuote(s) }

// shellQuote wraps an argument in single quotes for the remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
