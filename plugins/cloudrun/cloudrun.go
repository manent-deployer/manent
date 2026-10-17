// Package cloudrun releases a container image to a Google Cloud Run service: it changes the
// image in the service template, which creates a new revision, and sends all traffic to the
// latest ready revision. The release in service is the revision receiving traffic; rollback
// moves traffic to the previous revision, without recreating it.
//
// The artifact is a registry image (source oci://registry/repository:tag), always run pinned
// to the digest. The service (network, service account, scaling, variables) belongs to whoever
// manages the infrastructure (Terraform): the plugin changes only the image and writes version
// and deploy in the revision annotations. Every target is a region.
package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "cloudrun"

// Descriptor parameters (deploy.params) read by the plugin.
const (
	ParamProject           = "project"            // Google Cloud project (required)
	ParamService           = "service"            // Cloud Run service (default: application name)
	ParamContainer         = "container"          // container whose image changes (default: the only one, or the one named after the application)
	ParamCredentialsSecret = "credentials_secret" // secret with a service account JSON key; default: Application Default Credentials
	ParamWaitTimeout       = "wait_timeout"       // wait for the revision and the traffic (default 10m)
	defaultWaitTimeout     = 10 * time.Minute
)

// Annotations written on the template, so on every revision.
const (
	AnnotationVersion    = "manent.io/version"
	AnnotationDeployment = "manent.io/deployment"
)

// Traffic destination types.
const (
	trafficLatest   = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
	trafficRevision = "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION"
)

// DetailRevision is the prefix of the Release.Details keys with the release revision on each
// target ("revision@<target>").
const DetailRevision = "revision"

// ErrNotFound means a service or revision that does not exist.
var ErrNotFound = errors.New("not found")

var (
	regionRe  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	projectRe = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	serviceRe = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,47}[a-z0-9])?$`)
)

// Object is an API resource (service or revision) as generic JSON: the service is rewritten
// entirely, so fields the plugin does not know stay unchanged.
type Object = map[string]any

// Operation is a long-running API operation.
type Operation struct {
	Name  string     `json:"name"`
	Done  bool       `json:"done"`
	Error *OpFailure `json:"error,omitempty"`
}

// OpFailure is the error of an ended operation.
type OpFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// API are the Cloud Run operations (Admin API v2) used by the plugin. Names are the full
// resource names (projects/…/locations/…/services/…).
type API interface {
	GetService(ctx context.Context, name string) (Object, error)
	UpdateService(ctx context.Context, name string, svc Object, validateOnly bool) (*Operation, error)
	GetOperation(ctx context.Context, name string) (*Operation, error)
	GetRevision(ctx context.Context, name string) (Object, error)
	ListRevisions(ctx context.Context, service string) ([]Object, error)
}

// Connector opens access to the API with the request's credentials.
type Connector interface {
	Connect(ctx context.Context, req *plugin.Request) (API, error)
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
//   - endpoint: API endpoint (default https://run.googleapis.com)
//   - artifact_types: supported types (default ["cloudrun"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	endpoint := DefaultEndpoint
	if raw, ok := cfg["endpoint"]; ok {
		s, ok := raw.(string)
		if !ok || !(strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) {
			return nil, errors.New("endpoint must be an http(s) URL")
		}
		endpoint = s
	}
	return New(&HTTPConnector{Endpoint: endpoint}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

func serviceName(req *plugin.Request) string {
	if s := req.Params[ParamService]; s != "" {
		return s
	}
	return req.Application
}

// resource is the full name of the service in the region.
func resource(req *plugin.Request, region string) string {
	return "projects/" + req.Params[ParamProject] + "/locations/" + region + "/services/" + serviceName(req)
}

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes an annotation of the revision: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("the artifact is not a registry image: provide artifact.source oci://registry/repository:tag"))
	}
	if pr := req.Params[ParamProject]; !projectRe.MatchString(pr) {
		errs = append(errs, fmt.Errorf("invalid %s %q: provide the Google Cloud project ID", ParamProject, pr))
	}
	if s := serviceName(req); !serviceRe.MatchString(s) {
		errs = append(errs, fmt.Errorf("invalid service %q: lowercase letters, digits and '-', at most 49 characters (parameter %q)", s, ParamService))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("no region among the targets"))
	}
	for _, t := range req.Targets {
		if !regionRe.MatchString(t.Host) {
			errs = append(errs, fmt.Errorf("target %q: expected the name of a region (e.g. europe-west1)", t.Host))
		}
	}
	return errors.Join(errs...)
}

// get reads a nested value of o.
func get(o Object, keys ...string) any {
	var cur any = o
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func str(o Object, keys ...string) string {
	s, _ := get(o, keys...).(string)
	return s
}

// child returns the map o[key], creating it if missing.
func child(o Object, key string) Object {
	m, ok := o[key].(map[string]any)
	if !ok {
		m = Object{}
		o[key] = m
	}
	return m
}

// short returns the last element of a resource name.
func short(name string) string { return name[strings.LastIndexByte(name, '/')+1:] }

// containers returns the containers of the service template.
func containers(svc Object) []Object {
	raw, _ := get(svc, "template", "containers").([]any)
	var out []Object
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// chooseContainer chooses the container whose image changes.
func chooseContainer(svc Object, req *plugin.Request) (Object, error) {
	list := containers(svc)
	names := make([]string, len(list))
	for i, c := range list {
		names[i] = str(c, "name")
	}
	want := req.Params[ParamContainer]
	switch {
	case want != "":
	case len(list) == 1:
		return list[0], nil
	default:
		want = req.Application
	}
	if i := slices.Index(names, want); i >= 0 {
		return list[i], nil
	}
	return nil, fmt.Errorf("container %q missing from the service (containers: %s): provide %q", want, strings.Join(names, ", "), ParamContainer)
}

// serving returns the revision receiving traffic ("" if none). With split traffic (e.g. a
// percentage rollout in progress) it is the one with the larger share; a tie is an error.
func serving(svc Object) (string, error) {
	statuses, _ := svc["trafficStatuses"].([]any)
	var active []string
	rev, best := "", 0.0
	tie := false
	for _, s := range statuses {
		m, _ := s.(map[string]any)
		pct, _ := m["percent"].(float64)
		if pct <= 0 {
			continue
		}
		r, _ := m["revision"].(string)
		if m["type"] == trafficLatest || r == "" {
			r = str(svc, "latestReadyRevision")
		}
		r = short(r)
		active = append(active, fmt.Sprintf("%s %.0f%%", r, pct))
		switch {
		case pct > best:
			rev, best, tie = r, pct, false
		case pct == best && r != rev:
			tie = true
		}
	}
	if tie {
		return "", fmt.Errorf("traffic split equally among several revisions (%s)", strings.Join(active, ", "))
	}
	return rev, nil
}

// latestOnly reports whether the service already sends all traffic to the latest revision.
func latestOnly(svc Object) bool {
	tr, _ := svc["traffic"].([]any)
	if len(tr) != 1 {
		return false
	}
	m, _ := tr[0].(map[string]any)
	pct, _ := m["percent"].(float64)
	return m["type"] == trafficLatest && pct == 100
}

// state is the state of a target read before deploy, dry-run and rollback.
type state struct {
	region string
	name   string
	api    API
	svc    Object
}

func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	if !regionRe.MatchString(t.Host) {
		return nil, fmt.Errorf("invalid region %q", t.Host)
	}
	api, err := p.connector.Connect(ctx, req)
	if err != nil {
		return nil, err
	}
	name := resource(req, t.Host)
	svc, err := api.GetService(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("service %s missing: it must be created with the infrastructure (e.g. Terraform), manent updates its image", name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the service: %w", err)
	}
	return &state{region: t.Host, name: name, api: api, svc: svc}, nil
}

// version reads the manent version from the revision receiving traffic ("" if none).
func (s *state) version(ctx context.Context) (string, string, error) {
	rev, err := serving(s.svc)
	if err != nil || rev == "" {
		return "", "", err
	}
	r, err := s.api.GetRevision(ctx, s.name+"/revisions/"+rev)
	if err != nil {
		return "", rev, fmt.Errorf("reading revision %s: %w", rev, err)
	}
	return str(r, "annotations", AnnotationVersion), rev, nil
}

func (s *state) details(ctx context.Context, req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.region, "service": serviceName(req)}
	if v, _, err := s.version(ctx); err == nil && v != "" {
		d["previous"] = v
	}
	return d
}

// withImage returns the service with the release image and annotations and all traffic on the
// latest revision.
func withImage(svc Object, req *plugin.Request, image string) (Object, error) {
	c, err := chooseContainer(svc, req)
	if err != nil {
		return nil, err
	}
	c["image"] = image
	tmpl := child(svc, "template")
	ann := child(tmpl, "annotations")
	ann[AnnotationVersion], ann[AnnotationDeployment] = req.Artifact.Version, req.DeploymentID
	// A fixed revision name would prevent creating a new one: Cloud Run generates it.
	delete(tmpl, "revision")
	svc["traffic"] = []any{Object{"type": trafficLatest, "percent": 100}}
	return svc, nil
}

// Deploy releases the image to every region in sequence; it stops at the first error. It is
// idempotent: a template already updated by this deploy is not rewritten.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	res := &plugin.Result{Plugin: Name}
	for _, t := range req.Targets {
		details, err := p.deployTarget(ctx, t, req, image)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			return res, fmt.Errorf("region %s: %w", t.Host, err)
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: details})
	}
	return res, nil
}

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(ctx, req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.region, "service", serviceName(req))
	c, err := chooseContainer(s.svc, req)
	if err != nil {
		return details, err
	}
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	applied := str(c, "image") == image && str(s.svc, "template", "annotations", AnnotationDeployment) == req.DeploymentID && latestOnly(s.svc)
	if !applied {
		svc, err := withImage(s.svc, req, image)
		if err != nil {
			return details, err
		}
		op, err := s.api.UpdateService(ctx, s.name, svc, false)
		if err != nil {
			return details, fmt.Errorf("updating the service: %w", err)
		}
		log.InfoContext(ctx, "servizio aggiornato, in attesa della revisione", "image", image)
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	svc, err := s.api.GetService(ctx, s.name)
	if err != nil {
		return details, fmt.Errorf("reading the service: %w", err)
	}
	if st := str(svc, "terminalCondition", "state"); st != "" && st != "CONDITION_SUCCEEDED" {
		return details, fmt.Errorf("service in state %s: %s", st, str(svc, "terminalCondition", "message"))
	}
	created, ready := short(str(svc, "latestCreatedRevision")), short(str(svc, "latestReadyRevision"))
	if created != ready {
		return details, fmt.Errorf("revision %s not ready (%s stays in service)", created, ready)
	}
	details[DetailRevision] = ready
	if u := str(svc, "uri"); u != "" {
		details["url"] = u
	}
	log.InfoContext(ctx, "release attiva", "version", req.Artifact.Version, "revision", ready)
	return details, nil
}

// wait waits for the operation to end; a failed operation (e.g. a revision that does not
// start) is an error.
func (p *Plugin) wait(ctx context.Context, api API, op *Operation, timeout time.Duration) error {
	for !op.Done {
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %s not completed within %s", short(op.Name), timeout)
		case <-time.After(p.poll):
		}
		next, err := api.GetOperation(ctx, op.Name)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("operation %s not completed within %s", short(op.Name), timeout)
			}
			return fmt.Errorf("reading the operation: %w", err)
		}
		op = next
	}
	if op.Error != nil {
		return fmt.Errorf("operation failed: %s", op.Error.Message)
	}
	return nil
}

// CurrentRelease reads on every region the version of the revision receiving traffic. A
// revision without the manent annotation has no release; different versions are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "revisione che riceve il traffico"}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		v, rev, err := s.version(ctx)
		if err != nil {
			return nil, fmt.Errorf("region %s: %w", t.Host, err)
		}
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailRevision+"@"+t.Host] = rev
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

// Rollback sends all traffic of every region to the revision of the given release: the one in
// the release details or, failing that, the most recent with that version.
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
	details := map[string]string{"region": s.region, "service": serviceName(req)}
	rev := previous.Details[DetailRevision+"@"+t.Host]
	if rev == "" {
		if rev, err = p.findRevision(ctx, s, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailRevision] = rev
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if cur, err := serving(s.svc); err != nil || cur != rev || latestOnly(s.svc) {
		s.svc["traffic"] = []any{Object{"type": trafficRevision, "revision": rev, "percent": 100}}
		op, err := s.api.UpdateService(ctx, s.name, s.svc, false)
		if err != nil {
			return details, fmt.Errorf("shifting traffic: %w", err)
		}
		if err := p.wait(ctx, s.api, op, timeout); err != nil {
			return details, err
		}
	}
	p.logger.WarnContext(ctx, "release precedente ripristinata", "deployment", req.DeploymentID, "region", s.region,
		"service", serviceName(req), "version", previous.Version, "revision", rev, "rollback", true)
	return details, nil
}

// findRevision looks for the most recent revision with the given version.
func (p *Plugin) findRevision(ctx context.Context, s *state, version string) (string, error) {
	list, err := s.api.ListRevisions(ctx, s.name)
	if err != nil {
		return "", fmt.Errorf("listing the revisions: %w", err)
	}
	best, bestTime := "", ""
	for _, r := range list {
		if str(r, "annotations", AnnotationVersion) != version {
			continue
		}
		// createTime is RFC 3339 in UTC: lexicographic order is chronological.
		if ct := str(r, "createTime"); best == "" || ct > bestTime {
			best, bestTime = short(str(r, "name")), ct
		}
	}
	if best == "" {
		return "", fmt.Errorf("no revision of the service with version %s", version)
	}
	return best, nil
}

// DryRun checks on every region, without changing it, that service and container exist and
// that the update is accepted (validateOnly: API validation and permissions).
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, _ := plugin.ImageReference(req.Artifact)
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		details, err := p.checkTarget(ctx, t, req, image)
		if err != nil {
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: details})
			errs = append(errs, fmt.Errorf("region %s: %w", t.Host, err))
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusReady, Details: details})
	}
	return res, errors.Join(errs...)
}

func (p *Plugin) checkTarget(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(ctx, req)
	c, err := chooseContainer(s.svc, req)
	if err != nil {
		return details, err
	}
	details["container"] = str(c, "name")
	if image == "" {
		return details, nil
	}
	details["image"] = image
	svc, err := withImage(s.svc, req, image)
	if err != nil {
		return details, err
	}
	if _, err := s.api.UpdateService(ctx, s.name, svc, true); err != nil {
		return details, fmt.Errorf("checking the update: %w", err)
	}
	p.logger.InfoContext(ctx, "servizio pronto per il deploy", "deployment", req.DeploymentID, "region", s.region, "service", serviceName(req), "dry_run", true)
	return details, nil
}
