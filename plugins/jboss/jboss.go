// Package jboss releases Java EE / Jakarta EE applications (war, ear) to JBoss EAP and WildFly
// through the HTTP management API, in standalone or domain mode.
//
// Every version becomes a separate deployment, "<artifact>-<version>.<ext>" ("<tenant>.<artifact>-…"
// for the applications of a tenant), with the same runtime-name (default "<artifact>.<ext>",
// "<tenant>.<artifact>.<ext>" for a tenant):
// two artifacts of an application are two applications on the server. For the artifact named
// after the application, without tenant, the names are those of earlier releases. Switching from the version in service to the
// new one is a replace-deployment inside a composite operation, which the server applies
// atomically. Previous versions stay in the content repository, disabled, up to
// keep_releases: the rollback reactivates the previous one without uploading anything.
//
// Suspend (graceful), reload and restart of the server are controlled with boolean parameters.
package jboss

import (
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "jboss"

// Descriptor parameters (deploy.params) read by the plugin.
const (
	ParamUsername       = "username"        // management user
	ParamPasswordSecret = "password_secret" // secret with the management password
	ParamTLS            = "tls"             // https to the management interface (default true)
	ParamServerGroups   = "server_groups"   // domain mode: server groups, comma separated
	ParamRuntimeName    = "runtime_name"    // default "<artifact>.<ext>" ("<tenant>.<artifact>.<ext>" for a tenant)
	ParamSuspend        = "suspend"         // suspends the server before the version switch
	ParamSuspendTimeout = "suspend_timeout" // wait for requests in progress (default 60s)
	ParamReload         = "reload"          // server reload after the version switch
	ParamRestart        = "restart"         // JVM restart after the version switch
	ParamKeepReleases   = "keep_releases"   // versions kept, the active one included (default 3, at least 2)
	ParamWaitTimeout    = "wait_timeout"    // wait for the server after reload/restart (default 5m)
)

// DetailDeployment is the Release.Details key with the deployment name.
const DetailDeployment = "deployment"

const (
	defaultKeepReleases   = 3
	defaultSuspendTimeout = 60 * time.Second
	defaultWaitTimeout    = 5 * time.Minute
)

// extensions are the allowed artifact extensions.
var extensions = []string{".war", ".ear", ".jar", ".rar", ".sar"}

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector and Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	// poll is the read interval during waits.
	poll time.Duration
}

// New creates the plugin with the given connector and artifact types.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory builds the plugin from the application configuration:
//   - artifact_types: supported types (default ["war", "ear"])
//   - ca_file: extra CAs for the https management interface
//   - insecure_skip_tls_verify: does not verify the certificate (development only)
//   - request_timeout: maximum duration of a call (default 10m, for uploads)
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, "war", "ear")
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if f, _ := cfg["ca_file"].(string); f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no PEM certificate", f)
		}
		tlsCfg.RootCAs = pool
	}
	if v, _ := cfg["insecure_skip_tls_verify"].(bool); v {
		tlsCfg.InsecureSkipVerify = true
	}
	timeout := 10 * time.Minute
	if s, _ := cfg["request_timeout"].(string); s != "" {
		if timeout, err = time.ParseDuration(s); err != nil || timeout <= 0 {
			return nil, fmt.Errorf("invalid request_timeout %q", s)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	// A client that does not follow redirects: credentials go only to the given host.
	hc := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return New(HTTPConnector{HTTP: hc}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// options are the deploy parameters, already read and validated.
type options struct {
	username, passwordSecret string
	tls                      bool
	groups                   []string
	runtimeName, prefix, ext string
	suspend, reload, restart bool
	suspendTimeout, wait     time.Duration
	keep                     int
}

// parse reads and validates the request parameters.
func parse(req *plugin.Request) (options, error) {
	var errs []error
	// The release is tenant/application/artifact/environment: the artifact name (the application
	// for the artifact named after it) keeps two artifacts of an application apart, the tenant two
	// applications with the same name of different tenants. The environment is the server.
	base := cmp.Or(req.Artifact.Name, req.Application)
	o := options{username: req.Params[ParamUsername], passwordSecret: req.Params[ParamPasswordSecret], prefix: base + "-"}
	if req.Tenant != "" && req.Tenant != untenanted {
		o.prefix = req.Tenant + "." + o.prefix
	}
	if o.username == "" || o.passwordSecret == "" {
		errs = append(errs, fmt.Errorf("%s and %s are required", ParamUsername, ParamPasswordSecret))
	}
	boolean := func(key string, def bool) bool {
		raw := req.Params[key]
		if raw == "" {
			return def
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid %s %q: expected true or false", key, raw))
		}
		return v
	}
	o.tls = boolean(ParamTLS, true)
	o.suspend = boolean(ParamSuspend, false)
	o.reload = boolean(ParamReload, false)
	o.restart = boolean(ParamRestart, false)
	if o.reload && o.restart {
		errs = append(errs, fmt.Errorf("%s and %s are alternatives: restart already reloads the server", ParamReload, ParamRestart))
	}
	for _, g := range strings.Split(req.Params[ParamServerGroups], ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.groups = append(o.groups, g)
		}
	}
	var err error
	if o.suspendTimeout, err = pluginutil.Duration(req.Params, ParamSuspendTimeout, defaultSuspendTimeout); err != nil {
		errs = append(errs, err)
	}
	if o.wait, err = pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	o.keep = defaultKeepReleases
	if raw := req.Params[ParamKeepReleases]; raw != "" {
		if o.keep, err = strconv.Atoi(raw); err != nil || o.keep < 2 {
			errs = append(errs, fmt.Errorf("invalid %s %q: at least 2, to keep the previous version", ParamKeepReleases, raw))
		}
	}
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes part of the deployment name: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if !pluginutil.VersionRe.MatchString(base) {
		errs = append(errs, fmt.Errorf("artifact %q is not valid as a deployment name", base))
	}
	o.ext = artifactExt(req.Artifact)
	if o.ext == "" {
		errs = append(errs, fmt.Errorf("unrecognized artifact extension: allowed %s (from the file or from artifact.type)", strings.Join(extensions, ", ")))
	}
	// Two applications on a server cannot share a runtime-name (it gives the context root): the
	// default carries the tenant too, "<tenant>.<artifact>.<ext>".
	o.runtimeName = cmp.Or(req.Params[ParamRuntimeName], strings.TrimSuffix(o.prefix, "-")+o.ext)
	if strings.ContainsAny(o.runtimeName, "/\\") || !slices.Contains(extensions, strings.ToLower(filepath.Ext(o.runtimeName))) {
		errs = append(errs, fmt.Errorf("invalid %s %q: a file name with extension %s", ParamRuntimeName, o.runtimeName, strings.Join(extensions, ", ")))
	}
	for _, t := range req.Targets {
		if t.Host == "" || strings.ContainsAny(t.Host, "/?#@") {
			errs = append(errs, fmt.Errorf("invalid target %q: host of the management interface", t.Host))
		}
	}
	return o, errors.Join(errs...)
}

// artifactExt returns the artifact extension: from the downloaded file or from the type.
func artifactExt(a plugin.Artifact) string {
	if e := strings.ToLower(filepath.Ext(a.LocalPath)); slices.Contains(extensions, e) {
		return e
	}
	if e := "." + strings.ToLower(a.Type); slices.Contains(extensions, e) {
		return e
	}
	return ""
}

// untenanted is the tenant of the applications no tenant lists.
const untenanted = "untenanted"

// deploymentName is the deployment name of a version.
func (o options) deploymentName(version string) string { return o.prefix + version + o.ext }

// versionOf derives the version from the name of a deployment managed by manent.
func (o options) versionOf(name string) (string, bool) {
	if !strings.HasPrefix(name, o.prefix) || !strings.HasSuffix(name, o.ext) {
		return "", false
	}
	v := strings.TrimSuffix(strings.TrimPrefix(name, o.prefix), o.ext)
	return v, v != ""
}

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	_, err := parse(req)
	return err
}

// Deploy releases the version to every target in sequence and stops at the first error. A
// target already on the requested version is not touched: the operation is idempotent.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	sum, err := contentHash(req.Artifact.LocalPath)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, o, plugin.StatusDeployed, func(s *session) (map[string]string, error) {
		return p.release(ctx, s, req, o, o.deploymentName(req.Artifact.Version), sum)
	})
}

// Rollback reactivates on every target the deployment of the previous release, still present
// in the content repository.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	name := previous.Details[DetailDeployment]
	if name == "" {
		name = o.deploymentName(previous.Version)
	}
	return p.each(ctx, req, o, plugin.StatusRolledBack, func(s *session) (map[string]string, error) {
		return p.release(ctx, s, req, o, name, "")
	})
}

// CurrentRelease reads from the first target the active deployment with the application's
// runtime-name; nil if there is none.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	if len(req.Targets) == 0 {
		return nil, errors.New("no target")
	}
	s, err := p.open(ctx, req, o, req.Targets[0])
	if err != nil {
		return nil, err
	}
	deps, err := s.deployments(ctx)
	if err != nil {
		return nil, err
	}
	cur := active(deps, o.runtimeName)
	if cur == nil {
		return nil, nil
	}
	v, ok := o.versionOf(cur.Name)
	if !ok {
		// Deployment not created by manent (e.g. released by hand): the version is unknown, but the
		// rollback can still reactivate it by name.
		v = cur.Name
	}
	return &plugin.Release{Version: v, Details: map[string]string{DetailDeployment: cur.Name}}, nil
}

// DryRun checks credentials, mode, server groups and state of every target, without changes.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	o, err := parse(req)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, o, plugin.StatusReady, func(s *session) (map[string]string, error) {
		details := map[string]string{"mode": s.mode()}
		if !s.domain {
			state, err := s.attribute(ctx, nil, "server-state")
			if err != nil {
				return details, err
			}
			details["server_state"] = state
			if state != "running" {
				return details, fmt.Errorf("server in state %q", state)
			}
		}
		deps, err := s.deployments(ctx)
		if err != nil {
			return details, err
		}
		if cur := active(deps, o.runtimeName); cur != nil {
			details["current"] = cur.Name
		}
		details[DetailDeployment] = o.deploymentName(req.Artifact.Version)
		return details, nil
	})
}

// each opens a session per target and runs fn, stopping at the first error.
func (p *Plugin) each(ctx context.Context, req *plugin.Request, o options, status string, fn func(*session) (map[string]string, error)) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		s, err := p.open(ctx, req, o, t)
		var details map[string]string
		if err == nil {
			details, err = fn(s)
		}
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("target %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: status, Details: details})
	}
	return res, nil
}

// session is the connection to a standalone instance or a domain controller.
type session struct {
	c      Client
	domain bool
	groups []string
	host   string
	log    *slog.Logger
	poll   time.Duration
}

func (s *session) mode() string {
	if s.domain {
		return "domain"
	}
	return "standalone"
}

// open connects to the target, detects the mode and checks the server groups.
func (p *Plugin) open(ctx context.Context, req *plugin.Request, o options, t plugin.Target) (*session, error) {
	if req.Secrets == nil {
		return nil, errors.New("secret resolver not available")
	}
	pass, err := req.Secrets.Secret(ctx, o.passwordSecret)
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", o.passwordSecret, err)
	}
	scheme, port := "https", 9993
	if !o.tls {
		scheme, port = "http", 9990
	}
	if t.Port != 0 {
		port = t.Port
	}
	base := scheme + "://" + net.JoinHostPort(t.Host, strconv.Itoa(port))
	c, err := p.connector.Connect(ctx, base, o.username, pass)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", base, err)
	}
	s := &session{c: c, groups: o.groups, host: t.Host, poll: p.poll,
		log: p.logger.With("deployment", req.DeploymentID, "host", t.Host)}
	launch, err := s.attribute(ctx, nil, "launch-type")
	if err != nil {
		return nil, fmt.Errorf("management interface %s: %w", base, err)
	}
	switch launch {
	case "DOMAIN":
		s.domain = true
		if len(o.groups) == 0 {
			return nil, fmt.Errorf("%s is a domain controller: provide %s", t.Host, ParamServerGroups)
		}
		for _, g := range o.groups {
			if _, err := s.c.Execute(ctx, op("read-resource", Address{{"server-group", g}})); err != nil {
				return nil, fmt.Errorf("server group %s: %w", g, err)
			}
		}
	case "STANDALONE":
		if len(o.groups) > 0 {
			return nil, fmt.Errorf("%s is a standalone server: %s applies only in domain mode", t.Host, ParamServerGroups)
		}
	default:
		return nil, fmt.Errorf("launch-type %q not supported", launch)
	}
	return s, nil
}

// attribute reads a string attribute of a resource.
func (s *session) attribute(ctx context.Context, addr Address, name string) (string, error) {
	raw, err := s.c.Execute(ctx, op("read-attribute", addr, "name", name))
	if err != nil {
		return "", err
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("unreadable attribute %s: %s", name, raw)
	}
	return v, nil
}

// deployment is a deployment of the server (or of the domain).
type deployment struct {
	Name        string
	RuntimeName string
	Hash        string
	// Enabled: active (in domain mode, in all the given server groups).
	Enabled bool
	// Groups are the server groups it is assigned to (domain mode), with their enabled state.
	Groups map[string]bool
}

type dmrDeployment struct {
	RuntimeName string `json:"runtime-name"`
	Enabled     *bool  `json:"enabled"`
	Content     []struct {
		Hash struct {
			Bytes string `json:"BYTES_VALUE"`
		} `json:"hash"`
	} `json:"content"`
}

// deployments reads the deployments; in domain mode also the server group assignments.
func (s *session) deployments(ctx context.Context) (map[string]*deployment, error) {
	read := func(addr Address) (map[string]dmrDeployment, error) {
		raw, err := s.c.Execute(ctx, op("read-children-resources", addr, "child-type", "deployment"))
		if err != nil {
			return nil, err
		}
		out := map[string]dmrDeployment{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("unreadable deployment list: %w", err)
		}
		return out, nil
	}
	root, err := read(nil)
	if err != nil {
		return nil, err
	}
	out := map[string]*deployment{}
	for name, d := range root {
		dep := &deployment{Name: name, RuntimeName: cmp.Or(d.RuntimeName, name), Enabled: d.Enabled != nil && *d.Enabled}
		if len(d.Content) > 0 {
			dep.Hash = d.Content[0].Hash.Bytes
		}
		out[name] = dep
	}
	if !s.domain {
		return out, nil
	}
	for _, d := range out {
		d.Groups, d.Enabled = map[string]bool{}, true
	}
	for _, g := range s.groups {
		assigned, err := read(Address{{"server-group", g}})
		if err != nil {
			return nil, fmt.Errorf("server group %s: %w", g, err)
		}
		for name, a := range assigned {
			if d := out[name]; d != nil {
				d.Groups[g] = a.Enabled != nil && *a.Enabled
			}
		}
	}
	for _, d := range out {
		for _, g := range s.groups {
			d.Enabled = d.Enabled && d.Groups[g]
		}
	}
	return out, nil
}

// active returns the active deployment with the given runtime-name. In domain mode, if the
// assignments are not aligned, the one active in at least one server group.
func active(deps map[string]*deployment, runtimeName string) *deployment {
	var partial *deployment
	for _, name := range slices.Sorted(maps.Keys(deps)) {
		d := deps[name]
		if d.RuntimeName != runtimeName {
			continue
		}
		if d.Enabled {
			return d
		}
		for _, on := range d.Groups {
			if on && partial == nil {
				partial = d
			}
		}
	}
	return partial
}

// release brings the target to deployment name. With sum (deploy) it uploads the content if
// missing; without it (rollback) the deployment must already exist.
func (p *Plugin) release(ctx context.Context, s *session, req *plugin.Request, o options, name, sum string) (map[string]string, error) {
	details := map[string]string{"mode": s.mode(), DetailDeployment: name, "runtime_name": o.runtimeName}
	deps, err := s.deployments(ctx)
	if err != nil {
		return details, err
	}
	cur := active(deps, o.runtimeName)
	if cur != nil {
		details["previous"] = cur.Name
		if v, ok := o.versionOf(cur.Name); ok {
			details["previous_version"] = v
		}
		if cur.Name == name && cur.Enabled && (sum == "" || cur.Hash == sum) {
			s.log.InfoContext(ctx, "deployment already active", "name", name)
			details["unchanged"] = "true"
			return details, nil
		}
	}
	target := deps[name]
	switch {
	case target == nil && sum == "":
		return details, fmt.Errorf("deployment %s is no longer present: it cannot be reactivated", name)
	case target != nil && sum != "" && target.Hash != sum:
		// Same name but different content (e.g. artifact rebuilt with the same version): never
		// replace the content of the active deployment without going through a version switch.
		if target == cur {
			return details, fmt.Errorf("the active deployment %s has content different from the artifact: use a new version", name)
		}
		if err := s.remove(ctx, target); err != nil {
			return details, err
		}
		target = nil
	}
	if target == nil {
		if err := s.add(ctx, req.Artifact.LocalPath, name, o.runtimeName); err != nil {
			return details, err
		}
		s.log.InfoContext(ctx, "content uploaded", "name", name, "runtime_name", o.runtimeName)
		if deps, err = s.deployments(ctx); err != nil {
			return details, err
		}
		target = deps[name]
		if target == nil {
			return details, fmt.Errorf("deployment %s not present after the upload", name)
		}
	}

	suspended := false
	if o.suspend {
		if err := s.suspend(ctx, o.suspendTimeout); err != nil {
			return details, err
		}
		suspended = true
		details["suspended"] = "true"
	}
	if err := s.swap(ctx, deps, o.runtimeName, target); err != nil {
		if suspended {
			// The server rolled the switch back: put the previous version back in service.
			if rerr := s.resume(ctx); rerr != nil {
				s.log.ErrorContext(ctx, "resume after a failed version switch", "error", rerr)
				err = fmt.Errorf("%w; resume failed: %v", err, rerr)
			}
		}
		return details, err
	}
	s.log.InfoContext(ctx, "version activated", "name", name, "previous", details["previous"])
	switch {
	case o.restart:
		if err := s.restartServer(ctx, o.wait); err != nil {
			return details, err
		}
		details["restarted"] = "true"
	case o.reload:
		if err := s.reloadServer(ctx, o.wait); err != nil {
			return details, err
		}
		details["reloaded"] = "true"
	case suspended:
		if err := s.resume(ctx); err != nil {
			return details, err
		}
	}
	if sum != "" {
		if err := s.cleanup(ctx, o, name, details["previous"]); err != nil {
			// Cleanup does not fail a successful release.
			s.log.WarnContext(ctx, "cleanup of the previous deployments failed", "error", err)
		}
	}
	return details, nil
}

// add uploads the content and creates the deployment, disabled.
func (s *session) add(ctx context.Context, path, name, runtimeName string) error {
	hash, err := s.c.Upload(ctx, path)
	if err != nil {
		return fmt.Errorf("uploading %s: %w", filepath.Base(path), err)
	}
	content := []map[string]any{{"hash": map[string]string{"BYTES_VALUE": hash}}}
	params := []any{"content", content, "runtime-name", runtimeName}
	if !s.domain {
		params = append(params, "enabled", false)
	}
	if _, err := s.c.Execute(ctx, op("add", Address{{"deployment", name}}, params...)); err != nil {
		return err
	}
	for _, g := range s.groups {
		if _, err := s.c.Execute(ctx, op("add", Address{{"server-group", g}, {"deployment", name}}, "enabled", false)); err != nil {
			return fmt.Errorf("server group %s: %w", g, err)
		}
	}
	return nil
}

// swap activates target in place of the active deployment with the same runtime-name, in a
// single composite operation: if a step fails the server rolls back the others too. Where a
// deployment is active the step is replace-deployment: WildFly does not start a deployment
// enabled with "deploy" right after the "undeploy" of another one with the same runtime-name.
func (s *session) swap(ctx context.Context, deps map[string]*deployment, runtimeName string, target *deployment) error {
	// active returns the deployment with runtimeName active in group g ("" = standalone).
	active := func(g string) string {
		for _, name := range slices.Sorted(maps.Keys(deps)) {
			d := deps[name]
			if d == target || d.RuntimeName != runtimeName {
				continue
			}
			if g == "" && d.Enabled || g != "" && d.Groups[g] {
				return name
			}
		}
		return ""
	}
	var steps []Operation
	if !s.domain {
		if old := active(""); old != "" {
			steps = append(steps, op("replace-deployment", nil, "name", target.Name, "to-replace", old))
		} else {
			steps = append(steps, op("deploy", Address{{"deployment", target.Name}}))
		}
	}
	for _, g := range s.groups {
		on, assigned := target.Groups[g]
		switch old := active(g); {
		case on:
		case old != "" && assigned:
			steps = append(steps, op("replace-deployment", Address{{"server-group", g}}, "name", target.Name, "to-replace", old))
		case old != "":
			steps = append(steps, op("undeploy", Address{{"server-group", g}, {"deployment", old}}),
				op("add", Address{{"server-group", g}, {"deployment", target.Name}}, "enabled", true))
		case assigned:
			steps = append(steps, op("deploy", Address{{"server-group", g}, {"deployment", target.Name}}))
		default:
			steps = append(steps, op("add", Address{{"server-group", g}, {"deployment", target.Name}}, "enabled", true))
		}
	}
	if len(steps) == 0 {
		return nil
	}
	_, err := s.c.Execute(ctx, op("composite", nil, "steps", steps))
	if err != nil {
		return fmt.Errorf("version switch: %w", err)
	}
	return nil
}

// suspend suspends the server (or the server groups): new requests get 503 and those in
// progress have suspend_timeout to finish.
func (s *session) suspend(ctx context.Context, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if s.domain {
		for _, g := range s.groups {
			if _, err := s.c.Execute(ctx, op("suspend-servers", Address{{"server-group", g}}, "suspend-timeout", secs)); err != nil {
				return fmt.Errorf("suspend of server group %s: %w", g, err)
			}
		}
		s.log.InfoContext(ctx, "server groups suspended", "groups", s.groups)
		return nil
	}
	if _, err := s.c.Execute(ctx, op("suspend", nil, "suspend-timeout", secs)); err != nil {
		return fmt.Errorf("suspend: %w", err)
	}
	// On a standalone instance the operation may return before requests finish: wait for the
	// SUSPENDED state, at most suspend_timeout.
	deadline := time.Now().Add(timeout)
	for {
		state, err := s.attribute(ctx, nil, "suspend-state")
		if err == nil && state == "SUSPENDED" {
			s.log.InfoContext(ctx, "server suspended")
			return nil
		}
		if time.Now().After(deadline) {
			s.log.WarnContext(ctx, "server not yet suspended when suspend_timeout expired: going on", "state", state)
			return nil
		}
		if err := sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

// resume puts the server (or the server groups) back in service.
func (s *session) resume(ctx context.Context) error {
	if s.domain {
		for _, g := range s.groups {
			if _, err := s.c.Execute(ctx, op("resume-servers", Address{{"server-group", g}})); err != nil {
				return fmt.Errorf("resume of server group %s: %w", g, err)
			}
		}
		return nil
	}
	if _, err := s.c.Execute(ctx, op("resume", nil)); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

// reloadServer reloads configuration and services without restarting the JVM.
func (s *session) reloadServer(ctx context.Context, wait time.Duration) error {
	if s.domain {
		return s.groupOp(ctx, "reload-servers")
	}
	if _, err := s.c.Execute(ctx, op("reload", nil)); err != nil && !disconnected(err) {
		return fmt.Errorf("reload: %w", err)
	}
	return s.waitRunning(ctx, wait)
}

// restartServer restarts the JVM: in standalone mode with shutdown(restart=true), which
// requires a start through standalone.sh or a service that handles it.
func (s *session) restartServer(ctx context.Context, wait time.Duration) error {
	if s.domain {
		return s.groupOp(ctx, "restart-servers")
	}
	if _, err := s.c.Execute(ctx, op("shutdown", nil, "restart", true)); err != nil && !disconnected(err) {
		return fmt.Errorf("restart: %w", err)
	}
	return s.waitRunning(ctx, wait)
}

// groupOp runs a blocking operation on every server group.
func (s *session) groupOp(ctx context.Context, name string) error {
	for _, g := range s.groups {
		if _, err := s.c.Execute(ctx, op(name, Address{{"server-group", g}}, "blocking", true)); err != nil {
			return fmt.Errorf("%s of server group %s: %w", name, g, err)
		}
	}
	s.log.InfoContext(ctx, name+" completed", "groups", s.groups)
	return nil
}

// waitRunning waits until the server is running again; during the restart the management
// interface does not answer.
func (s *session) waitRunning(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	// The server takes a moment to leave the running state.
	if err := sleep(ctx, s.poll); err != nil {
		return err
	}
	for {
		state, err := s.attribute(ctx, nil, "server-state")
		if err == nil && state == "running" {
			s.log.InfoContext(ctx, "server running again")
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("server not running again within %s: %w", wait, err)
			}
			return fmt.Errorf("server not running again within %s: state %q", wait, state)
		}
		if err := sleep(ctx, s.poll); err != nil {
			return err
		}
	}
}

// remove deletes a disabled deployment (in domain mode from the server groups first).
func (s *session) remove(ctx context.Context, d *deployment) error {
	for g := range d.Groups {
		if _, err := s.c.Execute(ctx, op("remove", Address{{"server-group", g}, {"deployment", d.Name}})); err != nil {
			return fmt.Errorf("removing %s from server group %s: %w", d.Name, g, err)
		}
	}
	if _, err := s.c.Execute(ctx, op("remove", Address{{"deployment", d.Name}})); err != nil {
		return fmt.Errorf("removing %s: %w", d.Name, err)
	}
	return nil
}

// cleanup deletes the manent-managed versions beyond keep_releases, oldest first; it never
// touches the activated version, the previous one or an active deployment.
func (s *session) cleanup(ctx context.Context, o options, current, previous string) error {
	deps, err := s.deployments(ctx)
	if err != nil {
		return err
	}
	var old []*deployment
	for _, d := range deps {
		if _, ok := o.versionOf(d.Name); !ok || d.RuntimeName != o.runtimeName || d.Name == current || d.Name == previous || d.Enabled {
			continue
		}
		if slices.Contains(slices.Collect(maps.Values(d.Groups)), true) {
			continue
		}
		old = append(old, d)
	}
	keep := o.keep - 1
	if previous != "" {
		keep--
	}
	if len(old) <= keep {
		return nil
	}
	slices.SortFunc(old, func(a, b *deployment) int {
		va, _ := o.versionOf(a.Name)
		vb, _ := o.versionOf(b.Name)
		return compareVersions(va, vb)
	})
	for _, d := range old[:len(old)-keep] {
		if err := s.remove(ctx, d); err != nil {
			return err
		}
		s.log.InfoContext(ctx, "previous deployment removed", "name", d.Name)
	}
	return nil
}

// contentHash is the content hash as the server computes it (SHA-1, base64 in DMR).
func contentHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("artifact: %w", err)
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("artifact: %w", err)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// disconnected reports a network error: reload and shutdown may close the connection before
// the response.
func disconnected(err error) bool {
	var oe *OperationError
	return !errors.As(err, &oe)
}

// compareVersions compares two versions by numeric and text segments (1.10 > 1.9).
func compareVersions(a, b string) int {
	sa, sb := splitVersion(a), splitVersion(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		na, ea := strconv.Atoi(sa[i])
		nb, eb := strconv.Atoi(sb[i])
		var c int
		if ea == nil && eb == nil {
			c = cmp.Compare(na, nb)
		} else {
			c = strings.Compare(sa[i], sb[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(sa), len(sb))
}

func splitVersion(v string) []string {
	var out []string
	cur, digit := "", false
	for _, r := range v {
		if !unicode.IsDigit(r) && !unicode.IsLetter(r) {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if cur != "" && unicode.IsDigit(r) != digit {
			out = append(out, cur)
			cur = ""
		}
		cur += string(r)
		digit = unicode.IsDigit(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
