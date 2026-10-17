// Package lambda releases the code of an AWS Lambda function: it updates the code (container
// image or .zip package), publishes an immutable version and moves an alias to it. The release
// in service is the version the alias points to; rollback moves the alias back to the
// previous version, without uploading the code again.
//
// The function (role, memory, variables, triggers) belongs to whoever manages the
// infrastructure (Terraform, CloudFormation, SAM): the plugin changes only the code. Callers
// must invoke the alias (qualified ARN), not $LATEST. Every target is a region.
package lambda

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "lambda"

// Descriptor parameters (deploy.params) read by the plugin, besides the awsauth credential ones.
const (
	ParamFunction      = "function"     // function name (default: application name)
	ParamAlias         = "alias"        // alias moved to the new version (default "live")
	ParamWaitTimeout   = "wait_timeout" // wait for the update and the version (default 10m)
	defaultAlias       = "live"
	defaultWaitTimeout = 10 * time.Minute
	// maxZipSize is Lambda's limit for direct .zip upload.
	maxZipSize = 50 << 20
)

// Package types of a function.
const (
	PackageImage = "Image"
	PackageZip   = "Zip"
)

// DetailFunctionVersion is the prefix of the Release.Details keys with the function version
// on each target ("function_version@<target>").
const DetailFunctionVersion = "function_version"

// ErrNotFound means a function, version or alias that does not exist.
var ErrNotFound = errors.New("not found")

var (
	functionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	aliasRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	numericRe  = regexp.MustCompile(`^[0-9]+$`)
	ecrRe      = regexp.MustCompile(`^([0-9]{12}\.dkr\.ecr\.)([a-z0-9-]+)(\.amazonaws\.com(\.cn)?/)`)
)

// Function is the configuration of a function or of one of its versions.
type Function struct {
	Name             string
	PackageType      string
	State            string // Pending, Active, Inactive, Failed
	StateReason      string
	LastUpdateStatus string // InProgress, Successful, Failed
	LastUpdateReason string
	CodeSha256       string
	Version          string
	Description      string
}

// Alias is a function alias.
type Alias struct {
	Name            string
	FunctionVersion string
	Description     string
	// Weights are the additional versions with their traffic share (0-1).
	Weights map[string]float64
}

// Code is the new code: an image or a .zip package.
type Code struct {
	ImageURI string
	ZipFile  []byte
}

// API are the Lambda operations used by the plugin, in a region.
type API interface {
	// GetFunction reads the configuration of the function or, with qualifier, of a version.
	GetFunction(ctx context.Context, name, qualifier string) (*Function, error)
	UpdateCode(ctx context.Context, name string, code Code, dryRun bool) (*Function, error)
	PublishVersion(ctx context.Context, name, codeSha256, description string) (*Function, error)
	GetAlias(ctx context.Context, name, alias string) (*Alias, error)
	// PutAlias creates the alias or moves it to the version, clearing any traffic weights.
	PutAlias(ctx context.Context, name, alias, version, description string, create bool) error
	// RouteAlias leaves the alias on the version and sends the weights' versions their share.
	RouteAlias(ctx context.Context, name, alias, version, description string, weights map[string]float64) error
	ListVersions(ctx context.Context, name string) ([]Function, error)
}

// Connector opens access to Lambda in a region with the request's credentials.
type Connector interface {
	Connect(ctx context.Context, region string, req *plugin.Request) (API, error)
}

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector and Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	poll      time.Duration
}

// New creates the plugin with the given connector and supported artifact types.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 2 * time.Second}
}

// Factory builds the plugin from the application configuration:
//   - endpoint: alternative Lambda API endpoint (e.g. LocalStack)
//   - artifact_types: supported types (default ["lambda"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	endpoint, err := awsauth.Endpoint(cfg)
	if err != nil {
		return nil, err
	}
	return New(SDKConnector{Endpoint: endpoint}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

func functionName(req *plugin.Request) string {
	if f := req.Params[ParamFunction]; f != "" {
		return f
	}
	return req.Application
}

func aliasName(req *plugin.Request) string {
	if a := req.Params[ParamAlias]; a != "" {
		return a
	}
	return defaultAlias
}

func isImage(a plugin.Artifact) bool { return strings.HasSuffix(a.LocalPath, plugin.ImageRefExt) }

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and identifies the function version: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if req.Artifact.LocalPath != "" && !isImage(req.Artifact) && !strings.HasSuffix(strings.ToLower(req.Artifact.LocalPath), ".zip") {
		errs = append(errs, errors.New("the artifact must be a .zip package or a registry image (artifact.source oci://…)"))
	}
	if !functionRe.MatchString(functionName(req)) {
		errs = append(errs, fmt.Errorf("invalid %s %q: letters, digits, '_', '-' are allowed (at most 64)", ParamFunction, functionName(req)))
	}
	if a := aliasName(req); !aliasRe.MatchString(a) || numericRe.MatchString(a) {
		errs = append(errs, fmt.Errorf("invalid %s %q: letters, digits, '_', '-', not only digits", ParamAlias, a))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if err := awsauth.Validate(req.Params); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("no region among the targets"))
	}
	for _, t := range req.Targets {
		if !awsauth.ValidRegion(t.Host) {
			errs = append(errs, fmt.Errorf("target %q: expected the name of a region (e.g. eu-west-1)", t.Host))
		}
	}
	return errors.Join(errs...)
}

// regionalImage moves an ECR image to the target's region: Lambda accepts only images from a
// repository in the same region, and with ECR replication the digest is the same.
func regionalImage(ref, region string) string {
	return ecrRe.ReplaceAllString(ref, "${1}"+region+"${3}")
}

// code prepares the code for the region: the image pinned to the digest or the .zip content.
func code(a plugin.Artifact, region string) (Code, error) {
	if isImage(a) {
		ref, err := plugin.ImageReference(a)
		if err != nil {
			return Code{}, err
		}
		return Code{ImageURI: regionalImage(ref, region)}, nil
	}
	st, err := os.Stat(a.LocalPath)
	if err != nil {
		return Code{}, err
	}
	if st.Size() > maxZipSize {
		return Code{}, fmt.Errorf("package of %d MiB: Lambda accepts at most 50 MiB uploaded directly, use a container image", st.Size()>>20)
	}
	b, err := os.ReadFile(a.LocalPath)
	if err != nil {
		return Code{}, err
	}
	return Code{ZipFile: b}, nil
}

// description is the description of versions and aliases: the plugin reads the release back from it.
func description(version, deploymentID string) string {
	return "manent version=" + version + " deployment=" + deploymentID
}

// parseDescription reads version and deploy from a description written by manent.
func parseDescription(s string) (version, deploymentID string) {
	rest, ok := strings.CutPrefix(s, "manent ")
	if !ok {
		return "", ""
	}
	for _, f := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(f, "version="); ok {
			version = v
		}
		if v, ok := strings.CutPrefix(f, "deployment="); ok {
			deploymentID = v
		}
	}
	return version, deploymentID
}

// state is the state of a target read before deploy, dry-run and rollback.
type state struct {
	region string
	api    API
	fn     *Function
	alias  *Alias
}

func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	if !awsauth.ValidRegion(t.Host) {
		return nil, fmt.Errorf("invalid region %q", t.Host)
	}
	api, err := p.connector.Connect(ctx, t.Host, req)
	if err != nil {
		return nil, err
	}
	name := functionName(req)
	fn, err := api.GetFunction(ctx, name, "")
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("function %s missing: it must be created with the infrastructure (e.g. Terraform), manent updates its code", name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading function %s: %w", name, err)
	}
	want := PackageZip
	if isImage(req.Artifact) {
		want = PackageImage
	}
	if req.Artifact.LocalPath != "" && fn.PackageType != "" && fn.PackageType != want {
		return nil, fmt.Errorf("function %s uses %s packages, the artifact is of type %s", name, fn.PackageType, want)
	}
	alias, err := api.GetAlias(ctx, name, aliasName(req))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("reading alias %s: %w", aliasName(req), err)
	}
	return &state{region: t.Host, api: api, fn: fn, alias: alias}, nil
}

func (s *state) details(req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.region, "function": functionName(req), "alias": aliasName(req)}
	if s.alias != nil {
		if v, _ := parseDescription(s.alias.Description); v != "" {
			d["previous"] = v
		}
	}
	return d
}

// Deploy releases the code to every region in sequence; it stops at the first error. It is
// idempotent: if the alias already points to the version published by this deploy it does
// nothing, and publishing the same code again returns the existing version.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	name := functionName(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.region, "function", name)
	if s.alias != nil {
		if _, dep := parseDescription(s.alias.Description); dep == req.DeploymentID {
			details[DetailFunctionVersion] = s.alias.FunctionVersion
			log.InfoContext(ctx, "alias già sulla versione di questo deploy", "alias", aliasName(req), "function_version", s.alias.FunctionVersion)
			return details, nil
		}
	}
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := p.publish(ctx, s, req, details, log)
	if err != nil {
		return details, err
	}
	if err := s.api.PutAlias(ctx, name, aliasName(req), v, description(req.Artifact.Version, req.DeploymentID), s.alias == nil); err != nil {
		return details, fmt.Errorf("updating alias %s: %w", aliasName(req), err)
	}
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version, "alias", aliasName(req), "function_version", v)
	return details, nil
}

// publish updates the function code and publishes a version, without touching the alias.
// It returns the published function version.
func (p *Plugin) publish(ctx context.Context, s *state, req *plugin.Request, details map[string]string, log *slog.Logger) (string, error) {
	name := functionName(req)
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	c, err := code(req.Artifact, s.region)
	if err != nil {
		return "", err
	}
	// An update in progress (e.g. of the configuration) would fail the code one; a previously
	// failed one would not.
	if _, err := p.waitFunction(ctx, s.api, name, "", timeout, false, log); err != nil {
		return "", err
	}
	if _, err := s.api.UpdateCode(ctx, name, c, false); err != nil {
		return "", fmt.Errorf("updating the code: %w", err)
	}
	if c.ImageURI != "" {
		details["image"] = c.ImageURI
	}
	fn, err := p.waitFunction(ctx, s.api, name, "", timeout, true, log)
	if err != nil {
		return "", err
	}
	if c.ZipFile != nil {
		if want, err := hexToBase64(req.Artifact.SHA256); err == nil && want != "" && fn.CodeSha256 != want {
			return "", fmt.Errorf("sha256 of the uploaded code (%s) differs from that of the artifact (%s)", fn.CodeSha256, want)
		}
	}
	// CodeSha256 prevents publishing code changed meanwhile by another update.
	v, err := s.api.PublishVersion(ctx, name, fn.CodeSha256, description(req.Artifact.Version, req.DeploymentID))
	if err != nil {
		return "", fmt.Errorf("publishing the version: %w", err)
	}
	details[DetailFunctionVersion] = v.Version
	if _, err := p.waitFunction(ctx, s.api, name, v.Version, timeout, true, log); err != nil {
		return "", err
	}
	return v.Version, nil
}

func hexToBase64(h string) (string, error) {
	if h == "" {
		return "", nil
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// waitFunction waits until the function (or version qualifier) is active and without updates
// in progress. With strict, a failed update is an immediate error.
func (p *Plugin) waitFunction(ctx context.Context, api API, name, qualifier string, timeout time.Duration, strict bool, log *slog.Logger) (*Function, error) {
	what := "funzione " + name
	if qualifier != "" {
		what += " versione " + qualifier
	}
	last := ""
	for {
		fn, err := api.GetFunction(ctx, name, qualifier)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s not ready within %s (%s)", what, timeout, last)
			}
			return nil, fmt.Errorf("reading %s: %w", what, err)
		}
		switch {
		case fn.State == "Failed":
			return nil, fmt.Errorf("%s in state Failed: %s", what, fn.StateReason)
		case strict && fn.LastUpdateStatus == "Failed":
			return nil, fmt.Errorf("update of %s failed: %s", what, fn.LastUpdateReason)
		case fn.State != "Pending" && fn.LastUpdateStatus != "InProgress":
			return fn, nil
		}
		st := fn.State + "/" + fn.LastUpdateStatus
		if st != last {
			log.InfoContext(ctx, "in attesa della funzione", "function_version", qualifier, "state", fn.State, "last_update", fn.LastUpdateStatus)
			last = st
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s not ready within %s (%s)", what, timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// CurrentRelease reads on every region the version the alias points to. An alias missing or
// not created by manent has no release; different versions across regions are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "alias " + aliasName(req)}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if s.alias == nil {
			continue
		}
		v, _ := parseDescription(s.alias.Description)
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailFunctionVersion+"@"+t.Host] = s.alias.FunctionVersion
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v := range found {
			return &plugin.Release{Version: v, Details: details}, nil
		}
	}
	var desc []string
	for v, hosts := range found {
		desc = append(desc, v+" su "+strings.Join(hosts, ","))
	}
	sort.Strings(desc)
	return nil, fmt.Errorf("different versions across regions: %s", strings.Join(desc, "; "))
}

// Rollback moves every region's alias back to the function version of the given release: the
// one in the release details or, failing that, the most recent published with that version.
// It continues even if a region fails. It is idempotent.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !pluginutil.VersionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("invalid version %q", previous.Version)
	}
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.rollbackTarget(ctx, t, req, previous)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusRolledBack, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, req *plugin.Request, previous plugin.Release) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	name := functionName(req)
	if s.alias == nil {
		return details, fmt.Errorf("alias %s missing: no version to restore", aliasName(req))
	}
	fv := previous.Details[DetailFunctionVersion+"@"+t.Host]
	depID := ""
	if fv != "" {
		v, err := s.api.GetFunction(ctx, name, fv)
		if err != nil {
			return details, fmt.Errorf("reading version %s: %w", fv, err)
		}
		_, depID = parseDescription(v.Description)
	} else {
		if fv, depID, err = p.findVersion(ctx, s.api, name, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailFunctionVersion] = fv
	if s.alias.FunctionVersion != fv {
		if err := s.api.PutAlias(ctx, name, aliasName(req), fv, description(previous.Version, depID), false); err != nil {
			return details, fmt.Errorf("updating alias %s: %w", aliasName(req), err)
		}
	}
	p.logger.WarnContext(ctx, "release precedente ripristinata", "deployment", req.DeploymentID, "region", s.region,
		"function", name, "version", previous.Version, "function_version", fv, "rollback", true)
	return details, nil
}

// findVersion looks for the most recent function version published with the given version.
func (p *Plugin) findVersion(ctx context.Context, api API, name, version string) (string, string, error) {
	list, err := api.ListVersions(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("listing the versions of %s: %w", name, err)
	}
	best, bestN, depID := "", -1, ""
	for _, f := range list {
		v, dep := parseDescription(f.Description)
		n, err := strconv.Atoi(f.Version)
		if v != version || err != nil || n <= bestN {
			continue
		}
		best, bestN, depID = f.Version, n, dep
	}
	if best == "" {
		return "", "", fmt.Errorf("no version of %s published with version %s", name, version)
	}
	return best, depID, nil
}

// DryRun checks on every region, without changing it, that function and package type are the
// expected ones and that the credentials can update the code (UpdateFunctionCode with DryRun).
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, req)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, req *plugin.Request) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	details["package_type"] = s.fn.PackageType
	if s.alias == nil {
		details["alias_status"] = "da creare"
	}
	c, err := code(req.Artifact, s.region)
	if err != nil {
		return details, err
	}
	if c.ImageURI != "" {
		details["image"] = c.ImageURI
	}
	if _, err := s.api.UpdateCode(ctx, functionName(req), c, true); err != nil {
		return details, fmt.Errorf("checking the code update: %w", err)
	}
	p.logger.InfoContext(ctx, "funzione pronta per il deploy", "deployment", req.DeploymentID, "region", s.region, "function", functionName(req), "dry_run", true)
	return details, nil
}
