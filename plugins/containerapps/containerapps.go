// Package containerapps releases a container image to an Azure Container Apps app: it changes
// the image of the template, which creates a new revision with a suffix derived from the
// deploy, and waits for it to be running and healthy. The version is written in the
// MANENT_VERSION and MANENT_DEPLOYMENT environment variables of the container, so every
// revision carries it.
//
// With activeRevisionsMode Multiple all traffic goes to the new revision, the previous one
// stays active: the rollback moves traffic back to it, and percentage rollouts split the
// traffic between the stable revision and the candidate (testers reach it through a label).
// With Single the rollback creates a revision with the previous image.
//
// The app, its environment and its ingress belong to whoever manages the infrastructure
// (Terraform): manent changes only image, variables and traffic. Every target is an app: host
// "<name>" or "<resource group>/<name>".
package containerapps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/azure"
	"github.com/manent-deployer/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "containerapps"

const apiVersion = "2024-03-01"

// Parameters of the plugin, besides the shared Azure ones.
const (
	ParamContainer   = "container"    // container whose image changes (default: the only one, or the one named after the application)
	ParamWaitTimeout = "wait_timeout" // wait for the revision and the traffic (default 10m)
	defaultWait      = 10 * time.Minute
)

// Environment variables with the release, on the chosen container.
const (
	EnvVersion    = "MANENT_VERSION"
	EnvDeployment = "MANENT_DEPLOYMENT"
)

// Prefixes of the Release.Details keys ("<prefix>@<target>").
const (
	DetailRevision = "revision"
	DetailImage    = "image"
)

const (
	modeMultiple = "multiple"
)

var appRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// Object is an API resource as generic JSON.
type Object = map[string]any

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector, Rollbacker, SlotDeployer,
// TrafficRouter and TesterRouter.
type Plugin struct {
	settings azure.Settings
	auth     *azure.Auth
	types    []string
	logger   *slog.Logger
	poll     time.Duration
}

// New creates the plugin.
func New(s azure.Settings, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{settings: s, auth: &azure.Auth{Authority: s.Authority, IMDS: s.IMDS}, types: types, logger: logger, poll: 3 * time.Second}
}

// Factory builds the plugin from the configuration: artifact_types (default
// ["containerapps"]), management_endpoint, authority, imds_endpoint.
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	s, err := azure.SettingsFrom(cfg)
	if err != nil {
		return nil, err
	}
	return New(s, types, logger), nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer.
func (p *Plugin) Supports(t string) bool { return slices.Contains(p.types, t) }

// Validate implements plugin.Deployer.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes an environment variable of the revision: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("the artifact is not a registry image: provide artifact.source oci://registry/repository:tag"))
	}
	if !azure.SubscriptionRe.MatchString(req.Params[azure.ParamSubscription]) {
		errs = append(errs, fmt.Errorf("%s is required: the subscription ID", azure.ParamSubscription))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWait); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, azure.ValidateCredential(req.Params)...)
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("no container app among the targets"))
	}
	for _, t := range req.Targets {
		if _, err := azure.ParseTarget(t, req.Params, appRe); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// suffix is the revision suffix of a deploy (key: deploy ID, or deploy ID and version for the
// revision of a rollback): deterministic, so a retry finds the revision already created.
func suffix(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:])[:10]
}

func candidateSuffix(req *plugin.Request) string { return suffix("m", req.DeploymentID) }

// app is a container app of a target.
type app struct {
	p    *Plugin
	c    *azure.Client
	host string
	path string
	name string
	req  *plugin.Request
	obj  Object
	log  *slog.Logger
}

func (p *Plugin) open(ctx context.Context, req *plugin.Request, t plugin.Target) (*app, error) {
	tg, err := azure.ParseTarget(t, req.Params, appRe)
	if err != nil {
		return nil, err
	}
	cred, err := azure.CredentialFrom(ctx, req)
	if err != nil {
		return nil, err
	}
	a := &app{p: p, c: &azure.Client{Endpoint: p.settings.Management, Auth: p.auth, Cred: cred, Poll: p.poll}, host: t.Host, name: tg.Name, req: req,
		path: "/subscriptions/" + req.Params[azure.ParamSubscription] + "/resourceGroups/" + tg.ResourceGroup + "/providers/Microsoft.App/containerApps/" + tg.Name,
		log:  p.logger.With("deployment", req.DeploymentID, "app", tg.Name)}
	if err := a.refresh(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) refresh(ctx context.Context) error {
	var o Object
	if _, err := a.c.Do(ctx, http.MethodGet, a.path, apiVersion, nil, &o); err != nil {
		if errors.Is(err, azure.ErrNotFound) {
			return fmt.Errorf("container app %s missing: it must be created with the infrastructure (e.g. Terraform), manent updates its image", a.name)
		}
		return fmt.Errorf("reading container app %s: %w", a.name, err)
	}
	a.obj = o
	return nil
}

func get(o any, keys ...string) any {
	cur := o
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func str(o any, keys ...string) string {
	s, _ := get(o, keys...).(string)
	return s
}

func (a *app) mode() string {
	return strings.ToLower(str(a.obj, "properties", "configuration", "activeRevisionsMode"))
}

// ingress is the ingress of the app (nil without ingress).
func (a *app) ingress() Object {
	m, _ := get(a.obj, "properties", "configuration", "ingress").(map[string]any)
	return m
}

// template returns a copy of the template, which the plugin changes.
func (a *app) template() Object {
	b, _ := json.Marshal(get(a.obj, "properties", "template"))
	var t Object
	_ = json.Unmarshal(b, &t)
	if t == nil {
		t = Object{}
	}
	return t
}

func containers(tmpl Object) []Object {
	raw, _ := tmpl["containers"].([]any)
	var out []Object
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// chooseContainer chooses the container whose image changes.
func chooseContainer(tmpl Object, req *plugin.Request) (Object, error) {
	list := containers(tmpl)
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
	return nil, fmt.Errorf("container %q missing from the template (containers: %s): provide %q", want, strings.Join(names, ", "), ParamContainer)
}

// setEnv sets an environment variable of the container.
func setEnv(c Object, name, value string) {
	env, _ := c["env"].([]any)
	for _, e := range env {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			delete(m, "secretRef")
			m["value"] = value
			return
		}
	}
	c["env"] = append(env, Object{"name": name, "value": value})
}

func envOf(c Object, name string) string {
	env, _ := c["env"].([]any)
	for _, e := range env {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			s, _ := m["value"].(string)
			return s
		}
	}
	return ""
}

// releaseTemplate is the template with image, variables and revision suffix of a release.
func (a *app) releaseTemplate(image, version, deployID, sfx string) (Object, error) {
	tmpl := a.template()
	c, err := chooseContainer(tmpl, a.req)
	if err != nil {
		return nil, err
	}
	c["image"] = image
	setEnv(c, EnvVersion, version)
	setEnv(c, EnvDeployment, deployID)
	tmpl["revisionSuffix"] = sfx
	return tmpl, nil
}

// patch sends the changes and waits for the operation.
func (a *app) patch(ctx context.Context, props Object) error {
	hdr, err := a.c.Do(ctx, http.MethodPatch, a.path, apiVersion, Object{"properties": props}, nil)
	if err == nil {
		err = a.c.Wait(ctx, hdr)
	}
	if err != nil {
		return fmt.Errorf("updating container app %s: %w", a.name, err)
	}
	return a.refresh(ctx)
}

// withTraffic is the change of the ingress traffic (properties of a PATCH).
func (a *app) withTraffic(traffic []any) Object {
	b, _ := json.Marshal(a.ingress())
	var in Object
	_ = json.Unmarshal(b, &in)
	in["traffic"] = traffic
	return Object{"configuration": Object{"ingress": in}}
}

func (a *app) revision(ctx context.Context, name string) (Object, error) {
	var o Object
	if _, err := a.c.Do(ctx, http.MethodGet, a.path+"/revisions/"+name, apiVersion, nil, &o); err != nil {
		return nil, fmt.Errorf("reading revision %s: %w", name, err)
	}
	return o, nil
}

// waitRevision waits for the revision to be provisioned, running and healthy.
func (a *app) waitRevision(ctx context.Context, name string) error {
	last := ""
	for {
		rv, err := a.revision(ctx, name)
		if err != nil && !errors.Is(err, azure.ErrNotFound) {
			return err
		}
		if err == nil {
			prov, run, health := str(rv, "properties", "provisioningState"), str(rv, "properties", "runningState"), str(rv, "properties", "healthState")
			last = prov + "/" + run + "/" + health
			switch {
			case strings.EqualFold(prov, "Failed") || strings.EqualFold(run, "Failed") || strings.EqualFold(run, "Degraded") && strings.EqualFold(health, "Unhealthy"):
				return fmt.Errorf("revision %s not running (%s): %s", name, last, str(rv, "properties", "runningStateDetails"))
			case strings.EqualFold(prov, "Provisioned") && !strings.EqualFold(health, "Unhealthy") &&
				(strings.EqualFold(run, "Running") || strings.EqualFold(run, "RunningAtMaxScale") || strings.EqualFold(run, "Processing")):
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("revision %s not ready (%s): %w", name, last, context.Cause(ctx))
		case <-time.After(a.p.poll):
		}
	}
}

// serving is the revision receiving traffic: the largest weight of the ingress or, without
// ingress or in Single mode, the latest ready one. A tie is an error.
func (a *app) serving() (string, error) {
	latest := str(a.obj, "properties", "latestReadyRevisionName")
	in := a.ingress()
	if in == nil || a.mode() != modeMultiple {
		return latest, nil
	}
	traffic, _ := in["traffic"].([]any)
	best, w, tie := "", -1.0, false
	for _, t := range traffic {
		name := str(t, "revisionName")
		if get(t, "latestRevision") == true {
			name = latest
		}
		wt, _ := get(t, "weight").(float64)
		switch {
		case wt > w:
			best, w, tie = name, wt, false
		case wt == w && wt > 0 && name != best:
			tie = true
		}
	}
	if tie {
		return "", fmt.Errorf("traffic split equally among several revisions of %s", a.name)
	}
	return best, nil
}

// version reads version and image of a revision ("" if not released by manent).
func (a *app) version(ctx context.Context, rev string) (string, string, error) {
	if rev == "" {
		return "", "", nil
	}
	rv, err := a.revision(ctx, rev)
	if err != nil {
		return "", "", err
	}
	tmpl, _ := get(rv, "properties", "template").(map[string]any)
	c, err := chooseContainer(tmpl, a.req)
	if err != nil {
		return "", "", err
	}
	return envOf(c, EnvVersion), str(c, "image"), nil
}

func (p *Plugin) timeout(req *plugin.Request) time.Duration {
	d, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWait)
	return d
}

// each runs f on every target; with stop it ends at the first error.
func (p *Plugin) each(ctx context.Context, req *plugin.Request, stop bool, status string, f func(ctx context.Context, a *app) (map[string]string, error)) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		c, cancel := context.WithTimeout(ctx, p.timeout(req))
		a, err := p.open(c, req, t)
		var d map[string]string
		if err == nil {
			d, err = f(c, a)
		}
		cancel()
		if d == nil {
			d = map[string]string{}
		}
		if err != nil {
			d["error"] = err.Error()
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: d})
			errs = append(errs, fmt.Errorf("container app %s: %w", t.Host, err))
			if stop {
				return res, errs[0]
			}
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: status, Details: d})
	}
	return res, errors.Join(errs...)
}

// createRevision creates (if missing) the revision with the given suffix and waits for it;
// traffic, if not nil, replaces the ingress traffic in the same update.
func (a *app) createRevision(ctx context.Context, image, version, deployID, sfx string, traffic []any) (string, error) {
	rev := a.name + "--" + sfx
	if _, err := a.revision(ctx, rev); errors.Is(err, azure.ErrNotFound) {
		tmpl, err := a.releaseTemplate(image, version, deployID, sfx)
		if err != nil {
			return rev, err
		}
		props := Object{"template": tmpl}
		if traffic != nil && a.ingress() != nil && a.mode() == modeMultiple {
			props["configuration"] = a.withTraffic(traffic)["configuration"]
		}
		a.log.InfoContext(ctx, "new revision requested", "revision", rev, "image", image)
		if err := a.patch(ctx, props); err != nil {
			return rev, err
		}
	} else if err != nil {
		return rev, err
	}
	return rev, a.waitRevision(ctx, rev)
}

// Deploy implements plugin.Deployer: on every app in sequence, a new revision with all the
// traffic. It is idempotent: the revision of this deploy is created once.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.name, "mode": a.mode()}
		if prev, _ := a.serving(); prev != "" {
			if v, _, err := a.version(ctx, prev); err == nil && v != "" {
				d["previous"] = v
			}
		}
		rev, err := a.createRevision(ctx, image, req.Artifact.Version, req.DeploymentID, candidateSuffix(req), []any{Object{"latestRevision": true, "weight": 100}})
		d[DetailRevision] = rev
		if err != nil {
			return d, err
		}
		if err := a.trafficTo(ctx, rev, true); err != nil {
			return d, err
		}
		if f := str(a.ingress(), "fqdn"); f != "" {
			d["url"] = "https://" + f
		}
		a.log.InfoContext(ctx, "active release", "version", req.Artifact.Version, "revision", rev)
		return d, nil
	})
}

// trafficTo makes rev the revision receiving all traffic: in Multiple mode by setting the
// traffic (latest: as the latest revision), in Single mode by checking it is the latest ready.
func (a *app) trafficTo(ctx context.Context, rev string, latest bool) error {
	if a.ingress() == nil || a.mode() != modeMultiple {
		if r := str(a.obj, "properties", "latestReadyRevisionName"); r != rev {
			return fmt.Errorf("revision %s ready but %s is still the active one", rev, r)
		}
		return nil
	}
	if cur, err := a.serving(); err == nil && cur == rev && len(a.traffic()) == 1 {
		return nil
	}
	t := Object{"revisionName": rev, "weight": 100}
	if latest && str(a.obj, "properties", "latestRevisionName") == rev {
		t = Object{"latestRevision": true, "weight": 100}
	}
	return a.patch(ctx, a.withTraffic([]any{t}))
}

func (a *app) traffic() []any {
	t, _ := a.ingress()["traffic"].([]any)
	return t
}

// CurrentRelease implements plugin.ReleaseInspector: the version of the revision receiving
// traffic on every app; different versions are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "variable " + EnvVersion + " of the revision receiving the traffic"}
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		rev, err := a.serving()
		if err != nil {
			return nil, err
		}
		v, image, err := a.version(ctx, rev)
		if err != nil {
			return nil, fmt.Errorf("container app %s: %w", t.Host, err)
		}
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailRevision+"@"+t.Host], details[DetailImage+"@"+t.Host] = rev, image
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
	for _, v := range slices.Sorted(maps.Keys(found)) {
		desc = append(desc, v+" on "+strings.Join(found[v], ","))
	}
	return nil, fmt.Errorf("different versions on the container apps: %s", strings.Join(desc, "; "))
}

// Rollback implements plugin.Rollbacker: Multiple mode moves the traffic back to the previous
// revision (activated if needed); Single mode creates a revision with the previous image. An
// app the deploy has not changed is left alone. It continues after a failed app.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !pluginutil.VersionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("invalid version %q", previous.Version)
	}
	return p.each(ctx, req, false, plugin.StatusRolledBack, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.name, "version": previous.Version}
		cand := a.name + "--" + candidateSuffix(req)
		cur, _ := a.serving()
		if cur != cand && str(a.obj, "properties", "latestRevisionName") != cand {
			return d, nil
		}
		if a.ingress() != nil && a.mode() == modeMultiple {
			rev := previous.Details[DetailRevision+"@"+a.host]
			if rev == "" {
				var err error
				if rev, err = a.findRevision(ctx, previous.Version); err != nil {
					return d, err
				}
			}
			d[DetailRevision] = rev
			if err := a.activate(ctx, rev); err != nil {
				return d, err
			}
			if err := a.trafficTo(ctx, rev, false); err != nil {
				return d, err
			}
		} else {
			image := previous.Details[DetailImage+"@"+a.host]
			if image == "" {
				return d, fmt.Errorf("image of release %s unknown: rollback impossible", previous.Version)
			}
			rev, err := a.createRevision(ctx, image, previous.Version, req.DeploymentID, suffix("r", req.DeploymentID+"/"+previous.Version), nil)
			d[DetailRevision] = rev
			if err != nil {
				return d, err
			}
			if err := a.trafficTo(ctx, rev, true); err != nil {
				return d, err
			}
		}
		a.log.WarnContext(ctx, "previous release restored", "version", previous.Version, "revision", d[DetailRevision], "rollback", true)
		return d, nil
	})
}

// activate activates a revision, if inactive.
func (a *app) activate(ctx context.Context, rev string) error {
	rv, err := a.revision(ctx, rev)
	if err != nil {
		return err
	}
	if get(rv, "properties", "active") == true {
		return nil
	}
	if _, err := a.c.Do(ctx, http.MethodPost, a.path+"/revisions/"+rev+"/activate", apiVersion, nil, nil); err != nil {
		return fmt.Errorf("activating revision %s: %w", rev, err)
	}
	return a.waitRevision(ctx, rev)
}

// findRevision looks for the most recent revision with the given version.
func (a *app) findRevision(ctx context.Context, version string) (string, error) {
	var page struct {
		Value []Object `json:"value"`
	}
	if _, err := a.c.Do(ctx, http.MethodGet, a.path+"/revisions", apiVersion, nil, &page); err != nil {
		return "", fmt.Errorf("listing the revisions: %w", err)
	}
	best, bestTime := "", ""
	for _, rv := range page.Value {
		tmpl, _ := get(rv, "properties", "template").(map[string]any)
		c, err := chooseContainer(tmpl, a.req)
		if err != nil || envOf(c, EnvVersion) != version {
			continue
		}
		if ct := str(rv, "properties", "createdTime"); best == "" || ct > bestTime {
			best, bestTime = str(rv, "name"), ct
		}
	}
	if best == "" {
		return "", fmt.Errorf("no revision of %s with version %s", a.name, version)
	}
	return best, nil
}

// DryRun implements plugin.DryRunner: app, container and revision in service, without changes.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.each(ctx, req, false, plugin.StatusReady, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.name, "mode": a.mode()}
		c, err := chooseContainer(a.template(), req)
		if err != nil {
			return d, err
		}
		d["container"] = str(c, "name")
		rev, err := a.serving()
		if err != nil {
			return d, err
		}
		v, _, err := a.version(ctx, rev)
		if err != nil {
			return d, err
		}
		d[DetailRevision], d["current"] = rev, v
		if req.Traffic == nil && a.mode() != modeMultiple && a.ingress() != nil {
			d["note"] = "activeRevisionsMode Single: the rollback creates a new revision"
		}
		return d, nil
	})
}

// Percentage rollouts: revisions are the slots (activeRevisionsMode Multiple, with ingress).

const stateStable = "stable"

func (a *app) requireMultiple() error {
	if a.ingress() == nil || a.mode() != modeMultiple {
		return fmt.Errorf("strategy: traffic requires activeRevisionsMode Multiple and an ingress on container app %s", a.name)
	}
	return nil
}

func stableOf(req *plugin.Request, host string) string {
	if req.Traffic == nil {
		return ""
	}
	return req.Traffic.State[stateStable+"@"+host]
}

// split is the traffic between stable revision and candidate, with the testers' label.
func split(req *plugin.Request, stable, cand string, percent int) []any {
	c := Object{"revisionName": cand, "weight": percent}
	if req.Traffic != nil && req.Traffic.TesterTag != "" {
		c["label"] = req.Traffic.TesterTag
	}
	if stable == "" || stable == cand {
		c["weight"] = 100
		return []any{c}
	}
	return []any{Object{"revisionName": stable, "weight": 100 - percent}, c}
}

// PrepareSlots implements plugin.SlotDeployer: it records the revision receiving traffic.
func (p *Plugin) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	state := map[string]string{}
	has := false
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		if err := a.requireMultiple(); err != nil {
			return nil, err
		}
		rev, err := a.serving()
		if err != nil {
			return nil, err
		}
		if rev != "" {
			state[stateStable+"@"+t.Host] = rev
			has = true
		}
	}
	state[plugin.StateHasStable] = fmt.Sprint(has)
	state["candidate_suffix"] = candidateSuffix(req)
	return state, nil
}

// DeployCandidate implements plugin.SlotDeployer: the candidate revision, traffic held on the
// stable one.
func (p *Plugin) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	return p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		if err := a.requireMultiple(); err != nil {
			return nil, err
		}
		stable := stableOf(req, a.host)
		var hold []any
		if stable != "" {
			hold = []any{Object{"revisionName": stable, "weight": 100}}
		}
		rev, err := a.createRevision(ctx, image, req.Artifact.Version, req.DeploymentID, candidateSuffix(req), hold)
		d := map[string]string{"app": a.name, DetailRevision: rev, "stable": stable}
		if err != nil {
			return d, err
		}
		if w, ok := a.weight(rev); !ok || (stable == "" && w != 100) {
			pct := 0
			if stable == "" {
				pct = 100
			}
			if err := a.patch(ctx, a.withTraffic(split(req, stable, rev, pct))); err != nil {
				return d, err
			}
		}
		a.log.InfoContext(ctx, "candidate revision ready, without traffic", "revision", rev, "stable", stable)
		return d, nil
	})
}

// weight is the traffic share of a revision.
func (a *app) weight(rev string) (int, bool) {
	total, found := 0.0, false
	latest := str(a.obj, "properties", "latestReadyRevisionName")
	for _, t := range a.traffic() {
		name := str(t, "revisionName")
		if get(t, "latestRevision") == true {
			name = latest
		}
		if name != rev {
			continue
		}
		w, _ := get(t, "weight").(float64)
		total += w
		found = true
	}
	return int(math.Round(total)), found
}

// SetCandidateWeight implements plugin.TrafficRouter.
func (p *Plugin) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	_, err := p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		cand := a.name + "--" + candidateSuffix(req)
		if w, ok := a.weight(cand); ok && w == percent {
			return nil, nil
		}
		if err := a.patch(ctx, a.withTraffic(split(req, stableOf(req, a.host), cand, percent))); err != nil {
			return nil, err
		}
		a.log.InfoContext(ctx, "app traffic updated", "revision", cand, "percent", percent)
		return nil, nil
	})
	return err
}

// CandidateWeight implements plugin.TrafficRouter.
func (p *Plugin) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	weight := -2
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return 0, err
		}
		w, _ := a.weight(a.name + "--" + candidateSuffix(req))
		if weight != -2 && w != weight {
			return -1, nil
		}
		weight = w
	}
	return max(weight, 0), nil
}

// RouteTesters implements plugin.TesterRouter: the URL of the candidate's label,
// https://<app>---<label>.<environment domain>.
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if req.Traffic == nil || req.Traffic.TesterTag == "" {
		return nil, errors.New("rollout.traffic.testers.tag is required with the containerapps plugin")
	}
	out := map[string]string{}
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		fqdn := str(a.ingress(), "fqdn")
		name, domain, ok := strings.Cut(fqdn, ".")
		if !ok {
			return nil, fmt.Errorf("container app %s without ingress fqdn", t.Host)
		}
		out["tester_url@"+t.Host] = "https://" + name + "---" + req.Traffic.TesterTag + "." + domain
	}
	return out, nil
}

// PromoteCandidate implements plugin.SlotDeployer: all traffic to the candidate (as the latest
// revision, like after a normal deploy; the label is removed).
func (p *Plugin) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	_, err := p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		return nil, a.trafficTo(ctx, a.name+"--"+candidateSuffix(req), true)
	})
	return err
}

// RevertCandidate implements plugin.SlotDeployer: all traffic to the stable revision.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	_, err := p.each(ctx, req, false, plugin.StatusRolledBack, func(ctx context.Context, a *app) (map[string]string, error) {
		stable := stableOf(req, a.host)
		if stable == "" {
			return nil, nil
		}
		if err := a.activate(ctx, stable); err != nil {
			return nil, err
		}
		return nil, a.trafficTo(ctx, stable, false)
	})
	return err
}

// RetirePrevious implements plugin.SlotDeployer: it deactivates the replaced revision (it
// stops consuming replicas), unless it receives traffic again.
func (p *Plugin) RetirePrevious(ctx context.Context, req *plugin.Request) error {
	_, err := p.each(ctx, req, false, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		stable := stableOf(req, a.host)
		if stable == "" {
			return nil, nil
		}
		if w, ok := a.weight(stable); ok && w > 0 {
			return nil, nil
		}
		if _, err := a.c.Do(ctx, http.MethodPost, a.path+"/revisions/"+stable+"/deactivate", apiVersion, nil, nil); err != nil && !errors.Is(err, azure.ErrNotFound) {
			return nil, fmt.Errorf("deactivating revision %s: %w", stable, err)
		}
		return nil, nil
	})
	return err
}

var (
	_ plugin.DryRunner        = (*Plugin)(nil)
	_ plugin.ReleaseInspector = (*Plugin)(nil)
	_ plugin.Rollbacker       = (*Plugin)(nil)
	_ plugin.SlotDeployer     = (*Plugin)(nil)
	_ plugin.TrafficRouter    = (*Plugin)(nil)
	_ plugin.TesterRouter     = (*Plugin)(nil)
)
