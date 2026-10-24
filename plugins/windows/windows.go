// Package windows deploys to Windows hosts: web applications on IIS, Windows services,
// scheduled tasks and MSI packages, over WinRM or the OpenSSH server of Windows, with
// PowerShell scripts.
//
// Every version is extracted (from a .zip artifact) into <releases_dir>\<version>, or kept
// there as package.msi; the release in service is the physicalPath of the IIS application, the
// binPath of the service, the action of the scheduled task or the installed MSI product, so the
// switch is a configuration change (or an installation) and the rollback goes back to a kept
// directory. The package also provides the iis-arr and nlb traffic pools (pool.go).
package windows

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/remotecopy"
)

// Name is the plugin name.
const Name = "windows"

// Parameters of the plugin (deploy.params and environment configuration).
const (
	ParamMode        = "mode"         // iis (default), service, task or msi
	ParamReleasesDir = "releases_dir" // required: directory of the releases, e.g. C:\apps\shop\releases
	ParamSite        = "site"         // iis: site name (required)
	ParamApp         = "app"          // iis: application path, default "/" (the site)
	ParamAppPool     = "app_pool"     // iis: application pool to restart (optional)
	ParamStopAppPool = "stop_app_pool"
	ParamService     = "service"   // service: service name (required)
	ParamBinary      = "binary"    // service: executable relative to the release (required)
	ParamArguments   = "arguments" // service and task: arguments of the executable
	ParamTask        = "task"      // task: scheduled task name (required)
	ParamTaskPath    = "task_path" // task: folder of the task, default \
	ParamStopTask    = "stop_task" // task: stops a running instance instead of waiting for it
	ParamRunNow      = "run_now"   // task: starts the task after the switch
	ParamTaskUser    = "task_user" // task: account of the task, with task_password_secret
	ParamTaskSecret  = "task_password_secret"
	ParamProduct     = "product_name"       // msi: name of the installed product (required)
	ParamMSIProps    = "msi_properties"     // msi: public properties, "NAME=value NAME2=value"
	ParamUninstall   = "uninstall_previous" // msi: uninstalls the product before installing
	ParamTimeout     = "timeout"            // wait for the application pool or the service (default 60s)
	ParamKeep        = "keep_releases"
	ParamPostScript  = "post_script" // PowerShell run after the switch
	ParamTransport   = "transport"   // winrm (default) or ssh
	ParamShell       = "shell"       // powershell (default) or pwsh
	ParamPort        = "winrm_port"
	ParamHTTPS       = "winrm_https" // default true
	ParamAuth        = "winrm_auth"  // ntlm (default) or basic
	ParamInsecure    = "winrm_insecure_skip_verify"
	ParamPassword    = "password_secret" // secret with the password (WinRM, or SSH)
	ParamSSHKey      = "ssh_key_secret"  // secret with the SSH private key
)

// Values of the parameters.
const (
	ModeIIS        = "iis"
	ModeService    = "service"
	ModeTask       = "task"
	ModeMSI        = "msi"
	TransportSSH   = "ssh"
	TransportWinRM = "winrm"
	AuthNTLM       = "ntlm"
	AuthBasic      = "basic"
)

const defaultKeep = 5

var (
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	// nameRe accepts IIS site, application pool and service names without characters that could
	// leave the filter or the literal of the scripts.
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,127}$`)
	// taskPathRe is a task folder: \ or \Folder\Sub\.
	taskPathRe = regexp.MustCompile(`^\\([A-Za-z0-9][A-Za-z0-9 ._()-]{0,127}\\)*$`)
	// msiPropRe is a public MSI property with a value without spaces, quotes or shell characters.
	msiPropRe = regexp.MustCompile(`^[A-Z][A-Z0-9_.]{0,63}=[^\s"'` + "`" + `$;&|<>]{0,256}$`)
)

// settings are the validated parameters.
type settings struct {
	Mode, ReleasesDir          string
	Site, App, AppPool         string
	StopAppPool                bool
	Service, Binary, Arguments string
	Task, TaskPath, TaskUser   string
	TaskSecret                 string
	StopTask, RunNow           bool
	Product                    string
	MSIProps                   []string
	Uninstall                  bool
	Timeout                    time.Duration
	Keep                       int
	PostScript                 string
	Conn                       Conn
	PasswordSecret, SSHSecret  string
}

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector and Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
}

// New creates the plugin with the given connector and supported artifact types.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger}
}

// Factory builds the plugin from the configuration:
//   - artifact_types: supported types (default ["windows"]);
//   - winrm_ca_file: CA of the WinRM HTTPS certificates (default: system CAs);
//   - known_hosts, insecure_ignore_host_key: as remote-copy, for the ssh transport.
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types := []string{"windows"}
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
	caFile, _ := cfg["winrm_ca_file"].(string)
	ca, err := readCA(caFile)
	if err != nil {
		return nil, err
	}
	knownHosts, _ := cfg["known_hosts"].(string)
	insecure, _ := cfg["insecure_ignore_host_key"].(bool)
	c := DefaultConnector{SSH: &remotecopy.SSHDialer{KnownHostsPath: knownHosts, InsecureIgnoreHostKey: insecure}, CACert: ca}
	return New(c, types, logger), nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer.
func (p *Plugin) Supports(t string) bool { return slices.Contains(p.types, t) }

// Validate implements plugin.Deployer.
func (p *Plugin) Validate(req *plugin.Request) error {
	if _, err := parse(req.Params); err != nil {
		return err
	}
	var errs []error
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("at least one target"))
	}
	for _, t := range req.Targets {
		if t.Host == "" || t.User == "" {
			errs = append(errs, fmt.Errorf("target %q: host and user are required", t.Host))
		}
	}
	if !versionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, fmt.Errorf("version %q is not valid as a release name", req.Artifact.Version))
	}
	if req.Params[ParamMode] == ModeMSI && req.Artifact.LocalPath != "" && !strings.EqualFold(filepath.Ext(req.Artifact.LocalPath), ".msi") {
		errs = append(errs, errors.New("mode msi: the artifact must be a .msi package"))
	}
	return errors.Join(errs...)
}

// parse validates the parameters.
func parse(params map[string]string) (settings, error) {
	s := settings{
		Mode: cmp.Or(params[ParamMode], ModeIIS), ReleasesDir: params[ParamReleasesDir],
		Site: params[ParamSite], App: cmp.Or(params[ParamApp], "/"), AppPool: params[ParamAppPool],
		Service: params[ParamService], Binary: params[ParamBinary], Arguments: params[ParamArguments],
		PostScript: params[ParamPostScript], PasswordSecret: params[ParamPassword], SSHSecret: params[ParamSSHKey],
		Task: params[ParamTask], TaskPath: cmp.Or(params[ParamTaskPath], `\`), TaskUser: params[ParamTaskUser],
		TaskSecret: params[ParamTaskSecret], Product: params[ParamProduct], MSIProps: strings.Fields(params[ParamMSIProps]),
		Keep: defaultKeep, Timeout: time.Minute,
	}
	var errs []error
	bad := func(k, why string) { errs = append(errs, fmt.Errorf("parameter %s: %s", k, why)) }
	flag := func(k string, dst *bool) {
		if v := params[k]; v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				bad(k, "true or false")
			}
			*dst = b
		}
	}
	if s.ReleasesDir == "" || strings.ContainsAny(s.ReleasesDir, "'\"`$\r\n") {
		bad(ParamReleasesDir, "required, without quotes, $ or newlines")
	}
	switch s.Mode {
	case ModeIIS:
		if !nameRe.MatchString(s.Site) {
			bad(ParamSite, "required with mode iis (letters, digits, spaces, . _ - ( ))")
		}
		if !strings.HasPrefix(s.App, "/") || strings.ContainsAny(s.App, "'\"[]\r\n") {
			bad(ParamApp, "application path starting with /")
		}
		if s.AppPool != "" && !nameRe.MatchString(s.AppPool) {
			bad(ParamAppPool, "invalid name")
		}
		flag(ParamStopAppPool, &s.StopAppPool)
	case ModeService, ModeTask:
		if s.Mode == ModeService && !nameRe.MatchString(s.Service) {
			bad(ParamService, "required with mode service")
		}
		if s.Mode == ModeTask {
			if !nameRe.MatchString(s.Task) {
				bad(ParamTask, "required with mode task")
			}
			if !taskPathRe.MatchString(s.TaskPath) {
				bad(ParamTaskPath, `task folder, e.g. \ or \Shop\`)
			}
			flag(ParamStopTask, &s.StopTask)
			flag(ParamRunNow, &s.RunNow)
			if (s.TaskUser == "") != (s.TaskSecret == "") {
				bad(ParamTaskUser, "task_user and task_password_secret go together")
			}
			if strings.ContainsAny(s.TaskUser, "'\"`$\r\n") {
				bad(ParamTaskUser, "invalid account")
			}
		}
		if s.Binary == "" || strings.ContainsAny(s.Binary, "'\"`$\r\n") || strings.Contains(s.Binary, "..") {
			bad(ParamBinary, "executable relative to the release, required with mode "+s.Mode)
		}
		if strings.ContainsAny(s.Arguments, "\r\n") {
			bad(ParamArguments, "without newlines")
		}
	case ModeMSI:
		if !nameRe.MatchString(s.Product) {
			bad(ParamProduct, "required with mode msi: the name of the installed product (letters, digits, spaces, . _ - ( ))")
		}
		for _, p := range s.MSIProps {
			if !msiPropRe.MatchString(p) {
				bad(ParamMSIProps, fmt.Sprintf("%q: NAME=value with an uppercase public property, value without spaces or quotes", p))
			}
		}
		flag(ParamUninstall, &s.Uninstall)
	default:
		bad(ParamMode, "iis, service, task or msi")
	}
	if v := params[ParamTimeout]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad(ParamTimeout, "duration, e.g. 2m")
		}
		s.Timeout = d
	}
	if v := params[ParamKeep]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			bad(ParamKeep, "number >= 2 (the previous release is needed for rollback)")
		}
		s.Keep = n
	}
	s.Conn = parseConn(params, s.PasswordSecret, s.SSHSecret, bad)
	return s, errors.Join(errs...)
}

// parseConn validates the connection parameters, shared by the plugin and the pools.
func parseConn(params map[string]string, passwordSecret, sshSecret string, bad func(k, why string)) Conn {
	c := Conn{Transport: cmp.Or(params[ParamTransport], TransportWinRM), Shell: cmp.Or(params[ParamShell], "powershell"), HTTPS: true, Auth: cmp.Or(params[ParamAuth], AuthNTLM)}
	if c.Transport != TransportWinRM && c.Transport != TransportSSH {
		bad(ParamTransport, "winrm or ssh")
	}
	if c.Shell != "powershell" && c.Shell != "pwsh" {
		bad(ParamShell, "powershell or pwsh")
	}
	if c.Auth != AuthNTLM && c.Auth != AuthBasic {
		bad(ParamAuth, "ntlm or basic")
	}
	if v := params[ParamHTTPS]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(ParamHTTPS, "true or false")
		}
		c.HTTPS = b
	}
	if v := params[ParamInsecure]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(ParamInsecure, "true or false")
		}
		c.Insecure = b
	}
	c.Port = 5986
	if !c.HTTPS {
		c.Port = 5985
	}
	if v := params[ParamPort]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			bad(ParamPort, "port")
		}
		c.Port = n
	}
	if c.Transport == TransportWinRM && passwordSecret == "" {
		bad(ParamPassword, "required with transport winrm")
	}
	if c.Transport == TransportWinRM && c.Auth == AuthBasic && !c.HTTPS {
		bad(ParamAuth, "basic only with winrm_https: the password would travel in clear text")
	}
	if c.Transport == TransportSSH && passwordSecret == "" && sshSecret == "" {
		bad(ParamSSHKey, "ssh_key_secret or password_secret required with transport ssh")
	}
	return c
}

// connect resolves the secrets (only now, never kept) and opens the transport.
func (p *Plugin) connect(ctx context.Context, req *plugin.Request, s settings, t plugin.Target) (Transport, error) {
	c := s.Conn
	var err error
	if s.PasswordSecret != "" {
		if c.Password, err = req.Secrets.Secret(ctx, s.PasswordSecret); err != nil {
			return nil, err
		}
	}
	if s.SSHSecret != "" {
		if c.PrivateKey, err = req.Secrets.Secret(ctx, s.SSHSecret); err != nil {
			return nil, err
		}
	}
	tr, err := p.connector.Connect(ctx, t, c)
	if err != nil {
		return nil, fmt.Errorf("target %s: connection %s: %w", t.Host, c.Transport, err)
	}
	return tr, nil
}

// Deploy implements plugin.Deployer: on every target, one after the other, it copies and
// extracts the release (if not already there with the same sha256), puts it in service and
// removes the releases beyond keep_releases. It is idempotent.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	s, err := parse(req.Params)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		d := map[string]string{"version": req.Artifact.Version, "mode": s.Mode, "transport": s.Conn.Transport}
		if err := p.deployOne(ctx, req, s, t); err != nil {
			d["error"] = err.Error()
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: d})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: d})
	}
	return res, nil
}

func (p *Plugin) deployOne(ctx context.Context, req *plugin.Request, s settings, t plugin.Target) error {
	log := p.logger.With("target", t.Host, "version", req.Artifact.Version, "mode", s.Mode)
	tr, err := p.connect(ctx, req, s, t)
	if err != nil {
		return err
	}
	defer tr.Close()
	version, sha := req.Artifact.Version, req.Artifact.SHA256
	prev, err := p.current(ctx, tr, s)
	if err != nil {
		return err
	}
	out, err := tr.Run(ctx, probeScript(s, version, sha), nil)
	if err != nil {
		return fmt.Errorf("target check: %w", err)
	}
	if results(out)["present"] != "true" {
		f, err := os.Open(req.Artifact.LocalPath)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "copying the release to the target")
		_, err = tr.Run(ctx, uploadScript(s, version, sha), f)
		f.Close()
		if err != nil {
			return fmt.Errorf("copy and extraction: %w", err)
		}
	} else {
		log.InfoContext(ctx, "release already on the target, copy skipped")
	}
	pw, err := p.taskPassword(ctx, req, s)
	if err != nil {
		return err
	}
	out, err = tr.Run(ctx, activateScript(s, version, false, pw), nil)
	if err != nil {
		return fmt.Errorf("activation: %w", err)
	}
	if results(out)["reboot"] == "required" {
		log.WarnContext(ctx, "the MSI package requires a restart of the target, not done by manent")
	}
	log.InfoContext(ctx, "release live", "previous", prev)
	if _, err := tr.Run(ctx, cleanupScript(s, s.Keep, version, prev), nil); err != nil {
		// The release is in service: a failed cleanup does not fail the deploy.
		log.WarnContext(ctx, "cleanup of the releases failed", "error", err)
	}
	return nil
}

// taskPassword resolves the password of the scheduled task account, only when it is needed.
func (p *Plugin) taskPassword(ctx context.Context, req *plugin.Request, s settings) (string, error) {
	if s.Mode != ModeTask || s.TaskSecret == "" {
		return "", nil
	}
	return req.Secrets.Secret(ctx, s.TaskSecret)
}

// current returns the version in service on the target: the release directory the IIS
// application or the service points to, "" if it is not under releases_dir (not released by
// manent) or if there is none.
func (p *Plugin) current(ctx context.Context, tr Transport, s settings) (string, error) {
	out, err := tr.Run(ctx, currentScript(s), nil)
	if err != nil {
		return "", fmt.Errorf("reading the release in service: %w", err)
	}
	r := results(out)
	if r["found"] != "true" {
		switch s.Mode {
		case ModeIIS:
			return "", fmt.Errorf("application %s of site %s not found", s.App, s.Site)
		case ModeTask:
			return "", fmt.Errorf("scheduled task %s%s not found", s.TaskPath, s.Task)
		}
		return "", fmt.Errorf("service %s not found", s.Service)
	}
	p0 := r["path"]
	if s.Mode == ModeService || s.Mode == ModeTask {
		p0 = executable(p0)
	}
	return releaseOf(s.ReleasesDir, p0), nil
}

// executable extracts the executable from a service PathName ("C:\x\y.exe" -a or C:\x\y.exe -a).
func executable(pathName string) string {
	pathName = strings.TrimSpace(pathName)
	if strings.HasPrefix(pathName, `"`) {
		if i := strings.Index(pathName[1:], `"`); i >= 0 {
			return pathName[1 : i+1]
		}
	}
	if i := strings.IndexByte(pathName, ' '); i >= 0 {
		return pathName[:i]
	}
	return pathName
}

// releaseOf returns the release of a path under the releases directory (first segment after
// it), comparing without case and with / and \ as separators, as Windows does.
func releaseOf(root, p string) string {
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, `\`, "/")), "/") }
	r, q := norm(root), norm(p)
	if !strings.HasPrefix(q, r+"/") {
		return ""
	}
	rest := strings.ReplaceAll(p, `\`, "/")[len(r)+1:]
	v, _, _ := strings.Cut(rest, "/")
	if !versionRe.MatchString(v) {
		return ""
	}
	return v
}

// DryRun implements plugin.DryRunner: it connects to every target, checks PowerShell, the
// releases directory and the site or the service, and reads the release in service.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	s, err := parse(req.Params)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		tr, err := p.connect(ctx, req, s, t)
		if err != nil {
			return res, err
		}
		d := map[string]string{"mode": s.Mode, "transport": s.Conn.Transport}
		cur, err := p.current(ctx, tr, s)
		if err == nil {
			var out string
			if out, err = tr.Run(ctx, probeScript(s, req.Artifact.Version, req.Artifact.SHA256), nil); err == nil {
				d["powershell"], d["current"] = results(out)["powershell"], cur
			}
		}
		tr.Close()
		if err != nil {
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: d})
	}
	return res, nil
}

// CurrentRelease implements plugin.ReleaseInspector: the release in service on every target;
// a target without one (e.g. just added) is ignored, different versions are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	s, err := parse(req.Params)
	if err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for _, t := range req.Targets {
		tr, err := p.connect(ctx, req, s, t)
		if err != nil {
			return nil, err
		}
		v, err := p.current(ctx, tr, s)
		tr.Close()
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
			return &plugin.Release{Version: v, Details: map[string]string{"source": s.Mode, "targets": strings.Join(hosts, ",")}}, nil
		}
	}
	var desc []string
	for _, v := range slices.Sorted(maps.Keys(found)) {
		desc = append(desc, v+" on "+strings.Join(found[v], ","))
	}
	return nil, fmt.Errorf("different releases on the targets: %s", strings.Join(desc, "; "))
}

// Rollback implements plugin.Rollbacker: it puts the kept previous release back in service on
// every target, continuing after a failed target to restore as many as possible.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	s, err := parse(req.Params)
	if err != nil {
		return nil, err
	}
	if !versionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("version %q is not valid as a release name", previous.Version)
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		d := map[string]string{"version": previous.Version}
		pw, err := p.taskPassword(ctx, req, s)
		var tr Transport
		if err == nil {
			tr, err = p.connect(ctx, req, s, t)
		}
		if err == nil {
			_, err = tr.Run(ctx, activateScript(s, previous.Version, true, pw), nil)
			tr.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("target %s: %w", t.Host, err))
			d["error"] = err.Error()
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: d})
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: d})
		p.logger.WarnContext(ctx, "rollback completed on the target", "target", t.Host, "version", previous.Version)
	}
	return res, errors.Join(errs...)
}

var (
	_ plugin.DryRunner        = (*Plugin)(nil)
	_ plugin.ReleaseInspector = (*Plugin)(nil)
	_ plugin.Rollbacker       = (*Plugin)(nil)
)
