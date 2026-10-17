// Package appservice releases to Azure App Service web apps: a package (.zip, .war, .jar,
// .ear) through the OneDeploy API of Kudu, or a container image (linuxFxVersion or
// windowsFxVersion "DOCKER|<image>", pinned to the digest).
//
// With a deployment slot (parameter slot) the release goes to the slot, waits for its health
// check and swaps it with production: the previous version stays in the slot, so the rollback
// is a swap back, and a percentage rollout routes a share of production traffic to the slot
// (rampUpRules) before the swap. The version travels with the swap in the MANENT_VERSION and
// MANENT_DEPLOYMENT app settings of the slot. Without a slot the release goes straight to
// production and the version is written in the tags of the web app (no restart); a package
// cannot be rolled back there, an image can.
//
// The web app, its plan and its slots belong to whoever manages the infrastructure
// (Terraform): manent changes only the content and those settings. Every target is a web
// app: host "<name>" or "<resource group>/<name>".
package appservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/azure"
	"github.com/joeg-ita/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "appservice"

// apiVersion is the Microsoft.Web API version used.
const apiVersion = "2023-12-01"

// Parameters of the plugin, besides the shared Azure ones (subscription, resource_group,
// tenant_id, client_id, client_secret_secret).
const (
	ParamSlot        = "slot"         // deployment slot to release to and swap with production
	ParamHealthPath  = "health_path"  // path probed on the slot (or production) before the release is done
	ParamWaitTimeout = "wait_timeout" // wait for deployment, health and swap (default 15m)
	defaultWait      = 15 * time.Minute
)

// App settings (slot mode) and tags (production) with the release.
const (
	SettingVersion    = "MANENT_VERSION"
	SettingDeployment = "MANENT_DEPLOYMENT"
	TagVersion        = "manent-version"
	TagDeployment     = "manent-deployment"
	TagImage          = "manent-image"
	// TagPending marks production changed by a deploy not yet completed: the rollback knows it
	// has to restore it.
	TagPending = "manent-pending"
)

// DetailImage is the prefix of the Release.Details keys with the image of every target.
const DetailImage = "image"

var (
	siteRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,58}[A-Za-z0-9]$`)
	slotRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,58}$`)
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/?=&%-]{0,255}$`)
)

// packageTypes are the OneDeploy types by extension.
var packageTypes = map[string]string{".zip": "zip", ".war": "war", ".jar": "jar", ".ear": "ear"}

// Object is an API resource as generic JSON.
type Object = map[string]any

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector, Rollbacker, SlotDeployer,
// TrafficRouter and TesterRouter.
type Plugin struct {
	settings azure.Settings
	auth     *azure.Auth
	// scm and site replace https://<host> of Kudu and of the web app (lab, proxies).
	scm, site string
	http      *http.Client
	types     []string
	logger    *slog.Logger
	poll      time.Duration
}

// New creates the plugin.
func New(s azure.Settings, scm, site string, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{settings: s, auth: &azure.Auth{Authority: s.Authority, IMDS: s.IMDS}, scm: scm, site: site,
		http: &http.Client{Timeout: 30 * time.Minute}, types: types, logger: logger, poll: 3 * time.Second}
}

// Factory builds the plugin from the configuration:
//   - artifact_types: supported types (default ["appservice"]);
//   - management_endpoint, authority, imds_endpoint: endpoints (sovereign clouds);
//   - scm_endpoint, site_endpoint: base URLs replacing https://<host> of Kudu and of the web
//     apps (followed by /<host>), for proxies and tests.
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	s, err := azure.SettingsFrom(cfg)
	if err != nil {
		return nil, err
	}
	var scm, site string
	for k, dst := range map[string]*string{"scm_endpoint": &scm, "site_endpoint": &site} {
		if raw, ok := cfg[k]; ok {
			v, ok := raw.(string)
			if !ok || !(strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) {
				return nil, fmt.Errorf("%s must be an http(s) URL", k)
			}
			*dst = strings.TrimRight(v, "/")
		}
	}
	return New(s, scm, site, types, logger), nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer.
func (p *Plugin) Supports(t string) bool { return slices.Contains(p.types, t) }

// Validate implements plugin.Deployer.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes an app setting or a tag: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if lp := req.Artifact.LocalPath; lp != "" && !strings.HasSuffix(lp, plugin.ImageRefExt) {
		if _, ok := packageTypes[strings.ToLower(filepath.Ext(lp))]; !ok {
			errs = append(errs, errors.New("the artifact must be a .zip, .war, .jar or .ear package or a registry image (artifact.source oci://…)"))
		}
	}
	if !azure.SubscriptionRe.MatchString(req.Params[azure.ParamSubscription]) {
		errs = append(errs, fmt.Errorf("%s is required: the subscription ID", azure.ParamSubscription))
	}
	if s := req.Params[ParamSlot]; s != "" && (!slotRe.MatchString(s) || s == "production") {
		errs = append(errs, fmt.Errorf("invalid %s %q: lowercase letters, digits and '-', not production", ParamSlot, s))
	}
	if h := req.Params[ParamHealthPath]; h != "" && !pathRe.MatchString(h) {
		errs = append(errs, fmt.Errorf("invalid %s %q: a path starting with /", ParamHealthPath, h))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWait); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, azure.ValidateCredential(req.Params)...)
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("no web app among the targets"))
	}
	for _, t := range req.Targets {
		if _, err := azure.ParseTarget(t, req.Params, siteRe); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// app is a web app of a target, with the client of the request.
type app struct {
	p      *Plugin
	c      *azure.Client
	host   string
	sub    string
	tg     azure.Target
	slot   string
	req    *plugin.Request
	log    *slog.Logger
	cached map[string]Object
}

func (p *Plugin) open(ctx context.Context, req *plugin.Request, t plugin.Target) (*app, error) {
	tg, err := azure.ParseTarget(t, req.Params, siteRe)
	if err != nil {
		return nil, err
	}
	cred, err := azure.CredentialFrom(ctx, req)
	if err != nil {
		return nil, err
	}
	c := &azure.Client{Endpoint: p.settings.Management, Auth: p.auth, Cred: cred, Poll: p.poll}
	if p.scm != "" {
		c.Hosts = append(c.Hosts, p.scm)
	}
	return &app{p: p, c: c, host: t.Host, sub: req.Params[azure.ParamSubscription], tg: tg, slot: req.Params[ParamSlot], req: req,
		log: p.logger.With("deployment", req.DeploymentID, "app", tg.Name, "slot", req.Params[ParamSlot]), cached: map[string]Object{}}, nil
}

// res is the resource path of production ("") or of a slot.
func (a *app) res(slot string) string {
	r := "/subscriptions/" + a.sub + "/resourceGroups/" + a.tg.ResourceGroup + "/providers/Microsoft.Web/sites/" + a.tg.Name
	if slot != "" {
		r += "/slots/" + slot
	}
	return r
}

func where(slot string) string {
	if slot == "" {
		return "production"
	}
	return "slot " + slot
}

// get reads the web app or the slot.
func (a *app) get(ctx context.Context, slot string) (Object, error) {
	if o, ok := a.cached[slot]; ok {
		return o, nil
	}
	var o Object
	if _, err := a.c.Do(ctx, http.MethodGet, a.res(slot), apiVersion, nil, &o); err != nil {
		if errors.Is(err, azure.ErrNotFound) {
			if slot != "" {
				return nil, fmt.Errorf("slot %s of web app %s missing: it must be created with the infrastructure (e.g. Terraform)", slot, a.tg.Name)
			}
			return nil, fmt.Errorf("web app %s missing from resource group %s: it must be created with the infrastructure (e.g. Terraform), manent releases its content", a.tg.Name, a.tg.ResourceGroup)
		}
		return nil, fmt.Errorf("reading %s: %w", where(slot), err)
	}
	a.cached[slot] = o
	return o, nil
}

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

// hostName is the default host name of production or of the slot.
func (a *app) hostName(ctx context.Context, slot string) (string, error) {
	o, err := a.get(ctx, slot)
	if err != nil {
		return "", err
	}
	h := str(o, "properties", "defaultHostName")
	if h == "" {
		return "", fmt.Errorf("%s without defaultHostName", where(slot))
	}
	return h, nil
}

// siteURL is the URL of a path on production or on the slot.
func (a *app) siteURL(ctx context.Context, slot, path string) (string, error) {
	h, err := a.hostName(ctx, slot)
	if err != nil {
		return "", err
	}
	if a.p.site != "" {
		return a.p.site + "/" + h + path, nil
	}
	return "https://" + h + path, nil
}

// kudu is the base URL of the Kudu site (SCM) of production or of the slot.
func (a *app) kudu(ctx context.Context, slot string) (string, error) {
	o, err := a.get(ctx, slot)
	if err != nil {
		return "", err
	}
	hosts, _ := get(o, "properties", "enabledHostNames").([]any)
	for _, h := range hosts {
		if s, _ := h.(string); strings.Contains(s, ".scm.") {
			if a.p.scm != "" {
				return a.p.scm + "/" + s, nil
			}
			a.c.Hosts = append(a.c.Hosts, "https://"+s)
			return "https://" + s, nil
		}
	}
	return "", fmt.Errorf("%s without a Kudu (scm) host name", where(slot))
}

// appSettings reads the app settings. They may contain secrets: they stay in memory, only the
// manent ones are used.
func (a *app) appSettings(ctx context.Context, slot string) (map[string]string, error) {
	var out struct {
		Properties map[string]string `json:"properties"`
	}
	if _, err := a.c.Do(ctx, http.MethodPost, a.res(slot)+"/config/appsettings/list", apiVersion, nil, &out); err != nil {
		return nil, fmt.Errorf("reading the app settings of %s: %w", where(slot), err)
	}
	if out.Properties == nil {
		out.Properties = map[string]string{}
	}
	return out.Properties, nil
}

// markSlot writes version and deploy in the slot settings (the slot restarts), if needed.
func (a *app) markSlot(ctx context.Context, version, id string) error {
	s, err := a.appSettings(ctx, a.slot)
	if err != nil {
		return err
	}
	if s[SettingVersion] == version && s[SettingDeployment] == id {
		return nil
	}
	s[SettingVersion], s[SettingDeployment] = version, id
	if _, err := a.c.Do(ctx, http.MethodPut, a.res(a.slot)+"/config/appsettings", apiVersion, Object{"properties": s}, nil); err != nil {
		return fmt.Errorf("writing the app settings of %s: %w", where(a.slot), err)
	}
	return nil
}

// tags reads the tags of the web app.
func (a *app) tags(ctx context.Context) (map[string]string, error) {
	o, err := a.get(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if m, ok := o["tags"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out, nil
}

// setTags merges the tags of the web app (Microsoft.Resources/tags: no restart); an empty
// value removes the tag.
func (a *app) setTags(ctx context.Context, tags map[string]string) error {
	merge, del := map[string]string{}, map[string]string{}
	for k, v := range tags {
		if v == "" {
			del[k] = ""
		} else {
			merge[k] = v
		}
	}
	for op, m := range map[string]map[string]string{"Merge": merge, "Delete": del} {
		if len(m) == 0 {
			continue
		}
		body := Object{"operation": op, "properties": Object{"tags": m}}
		if _, err := a.c.Do(ctx, http.MethodPatch, a.res("")+"/providers/Microsoft.Resources/tags/default", "2021-04-01", body, nil); err != nil {
			return fmt.Errorf("updating the tags of web app %s: %w", a.tg.Name, err)
		}
	}
	delete(a.cached, "")
	return nil
}

// webConfig reads the site configuration of production or of the slot.
func (a *app) webConfig(ctx context.Context, slot string) (Object, error) {
	var o Object
	if _, err := a.c.Do(ctx, http.MethodGet, a.res(slot)+"/config/web", apiVersion, nil, &o); err != nil {
		return nil, fmt.Errorf("reading the configuration of %s: %w", where(slot), err)
	}
	return o, nil
}

func (a *app) patchWebConfig(ctx context.Context, slot string, props Object) error {
	hdr, err := a.c.Do(ctx, http.MethodPatch, a.res(slot)+"/config/web", apiVersion, Object{"properties": props}, nil)
	if err == nil {
		err = a.c.Wait(ctx, hdr)
	}
	if err != nil {
		return fmt.Errorf("updating the configuration of %s: %w", where(slot), err)
	}
	return nil
}

// fxField is the configuration field with the container image: windowsFxVersion for Windows
// containers, linuxFxVersion otherwise.
func fxField(cfg Object) string {
	if strings.HasPrefix(strings.ToUpper(str(cfg, "properties", "windowsFxVersion")), "DOCKER|") {
		return "windowsFxVersion"
	}
	return "linuxFxVersion"
}

// runningImage is the image the configuration runs ("" if it is not a container).
func runningImage(cfg Object) string {
	v := str(cfg, "properties", fxField(cfg))
	if len(v) > 7 && strings.EqualFold(v[:7], "DOCKER|") {
		return v[7:]
	}
	return ""
}

// deployContent releases the artifact to production or to the slot.
func (a *app) deployContent(ctx context.Context, slot string) error {
	if isImage(a.req) {
		image, err := plugin.ImageReference(a.req.Artifact)
		if err != nil {
			return err
		}
		cfg, err := a.webConfig(ctx, slot)
		if err != nil {
			return err
		}
		if runningImage(cfg) == image {
			return nil
		}
		a.log.InfoContext(ctx, "immagine del container aggiornata", "where", where(slot), "image", image)
		return a.patchWebConfig(ctx, slot, Object{fxField(cfg): "DOCKER|" + image})
	}
	return a.publish(ctx, slot)
}

// isImage reports whether the artifact is a registry image (a reference, not a package).
func isImage(req *plugin.Request) bool {
	return strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt)
}

// kuduDeployment is a deployment record of Kudu.
type kuduDeployment struct {
	ID         string `json:"id"`
	Status     int    `json:"status"`
	StatusText string `json:"status_text"`
	Complete   bool   `json:"complete"`
	LogURL     string `json:"log_url"`
}

// Kudu deployment statuses.
const (
	kuduFailed  = 3
	kuduSuccess = 4
)

// publish uploads the package with OneDeploy (asynchronous) and waits for the new deployment
// record of Kudu.
func (a *app) publish(ctx context.Context, slot string) error {
	base, err := a.kudu(ctx, slot)
	if err != nil {
		return err
	}
	typ := packageTypes[strings.ToLower(filepath.Ext(a.req.Artifact.LocalPath))]
	var before kuduDeployment
	if _, err := a.c.Raw(ctx, http.MethodGet, base+"/api/deployments/latest", nil, "", &before); err != nil && !errors.Is(err, azure.ErrNotFound) {
		return fmt.Errorf("Kudu of %s: %w", where(slot), err)
	}
	f, err := os.Open(a.req.Artifact.LocalPath)
	if err != nil {
		return err
	}
	defer f.Close()
	a.log.InfoContext(ctx, "pacchetto inviato a Kudu (OneDeploy)", "where", where(slot), "type", typ)
	if _, err := a.c.Raw(ctx, http.MethodPost, base+"/api/publish?type="+typ+"&async=true", f, "application/octet-stream", nil); err != nil {
		return fmt.Errorf("OneDeploy on %s: %w", where(slot), err)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("Kudu deployment on %s not completed: %w", where(slot), context.Cause(ctx))
		case <-time.After(a.p.poll):
		}
		var d kuduDeployment
		if _, err := a.c.Raw(ctx, http.MethodGet, base+"/api/deployments/latest", nil, "", &d); err != nil {
			if errors.Is(err, azure.ErrNotFound) {
				continue
			}
			return fmt.Errorf("Kudu of %s: %w", where(slot), err)
		}
		if d.ID == before.ID || !d.Complete {
			continue
		}
		if d.Status != kuduSuccess {
			return fmt.Errorf("Kudu deployment %s on %s failed (status %d): %s %s", d.ID, where(slot), d.Status, d.StatusText, d.LogURL)
		}
		return nil
	}
}

// health waits for health_path to answer 2xx on production or on the slot.
func (a *app) health(ctx context.Context, slot string) error {
	path := a.req.Params[ParamHealthPath]
	if path == "" {
		return nil
	}
	u, err := a.siteURL(ctx, slot, path)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	last := ""
	for {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(r)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s of %s not healthy (%s): %w", path, where(slot), last, context.Cause(ctx))
		case <-time.After(a.p.poll):
		}
	}
}

// swap exchanges the slot with production and waits for the operation.
func (a *app) swap(ctx context.Context) error {
	a.log.InfoContext(ctx, "swap dello slot con la produzione")
	hdr, err := a.c.Do(ctx, http.MethodPost, a.res(a.slot)+"/slotsswap", apiVersion, Object{"targetSlot": "production", "preserveVnet": true}, nil)
	if err == nil {
		err = a.c.Wait(ctx, hdr)
	}
	if err != nil {
		return fmt.Errorf("swap of slot %s with production: %w", a.slot, err)
	}
	clear(a.cached)
	return nil
}

func (p *Plugin) timeout(req *plugin.Request) time.Duration {
	d, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWait)
	return d
}

// each runs f on every target in sequence; with stop it ends at the first error, otherwise it
// continues (rollback, dry-run). status is the status of the targets done.
func (p *Plugin) each(ctx context.Context, req *plugin.Request, stop bool, status string, f func(ctx context.Context, a *app) (map[string]string, error)) (*plugin.Result, error) {
	res := &plugin.Result{Plugin: Name}
	var errs []error
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		var d map[string]string
		if err == nil {
			c, cancel := context.WithTimeout(ctx, p.timeout(req))
			d, err = f(c, a)
			cancel()
		}
		if d == nil {
			d = map[string]string{}
		}
		if err != nil {
			d["error"] = err.Error()
			res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: d})
			errs = append(errs, fmt.Errorf("web app %s: %w", t.Host, err))
			if stop {
				return res, errs[0]
			}
			continue
		}
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.Host, Status: status, Details: d})
	}
	return res, errors.Join(errs...)
}

// Deploy implements plugin.Deployer: on every web app in sequence, to the slot and then the
// swap, or straight to production. It is idempotent: a swap already done by this deploy is
// not repeated.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.tg.Name, "version": a.req.Artifact.Version}
		if _, err := a.get(ctx, ""); err != nil {
			return d, err
		}
		if a.slot != "" {
			if _, err := a.get(ctx, a.slot); err != nil {
				return d, err
			}
		}
		if a.slot == "" {
			d["mode"] = "production"
			return d, a.deployProduction(ctx)
		}
		d["mode"], d["slot"] = "slot", a.slot
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return d, err
		}
		if prod[SettingDeployment] == req.DeploymentID {
			return d, nil
		}
		if err := a.releaseToSlot(ctx); err != nil {
			return d, err
		}
		if err := a.swap(ctx); err != nil {
			return d, err
		}
		if prod, err = a.appSettings(ctx, ""); err != nil {
			return d, err
		}
		if prod[SettingDeployment] != req.DeploymentID {
			return d, fmt.Errorf("swap completed but production reports deploy %q: is %s a slot setting (sticky)?", prod[SettingDeployment], SettingDeployment)
		}
		a.log.InfoContext(ctx, "release in esercizio dopo lo swap", "version", req.Artifact.Version, "previous", prod[SettingVersion])
		return d, nil
	})
}

// deployProduction releases straight to production, then writes the tags.
func (a *app) deployProduction(ctx context.Context) error {
	if err := a.setTags(ctx, map[string]string{TagPending: a.req.DeploymentID}); err != nil {
		return err
	}
	if err := a.deployContent(ctx, ""); err != nil {
		return err
	}
	if err := a.health(ctx, ""); err != nil {
		return err
	}
	image, _ := plugin.ImageReference(a.req.Artifact)
	a.log.InfoContext(ctx, "release in esercizio", "version", a.req.Artifact.Version)
	return a.setTags(ctx, map[string]string{TagVersion: a.req.Artifact.Version, TagDeployment: a.req.DeploymentID, TagImage: image, TagPending: ""})
}

// releaseToSlot writes the settings, releases the content to the slot and waits for its health.
func (a *app) releaseToSlot(ctx context.Context) error {
	if err := a.markSlot(ctx, a.req.Artifact.Version, a.req.DeploymentID); err != nil {
		return err
	}
	if err := a.deployContent(ctx, a.slot); err != nil {
		return err
	}
	if err := a.health(ctx, a.slot); err != nil {
		return err
	}
	a.log.InfoContext(ctx, "release pronta nello slot", "version", a.req.Artifact.Version)
	return nil
}

// current reads the version in service on the web app ("" if none) and the image.
func (a *app) current(ctx context.Context) (string, string, error) {
	cfg, err := a.webConfig(ctx, "")
	if err != nil {
		return "", "", err
	}
	image := runningImage(cfg)
	if a.slot != "" {
		s, err := a.appSettings(ctx, "")
		if err != nil {
			return "", "", err
		}
		return s[SettingVersion], image, nil
	}
	tags, err := a.tags(ctx)
	if err != nil {
		return "", "", err
	}
	// An image changed outside manent is no release of manent.
	if tags[TagImage] != "" && tags[TagImage] != image {
		return "", image, nil
	}
	return tags[TagVersion], image, nil
}

// CurrentRelease implements plugin.ReleaseInspector: the version in production on every web
// app; different versions are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "app setting " + SettingVersion}
	if req.Params[ParamSlot] == "" {
		details["source"] = "tag " + TagVersion
	}
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		v, image, err := a.current(ctx)
		if err != nil {
			return nil, fmt.Errorf("web app %s: %w", t.Host, err)
		}
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		if image != "" {
			details[DetailImage+"@"+t.Host] = image
		}
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
	return nil, fmt.Errorf("different versions on the web apps: %s", strings.Join(desc, "; "))
}

// Rollback implements plugin.Rollbacker: with a slot it swaps back, if the deploy had already
// swapped; without a slot it puts the previous image back. It continues after a failed web app.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !pluginutil.VersionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("invalid version %q", previous.Version)
	}
	return p.each(ctx, req, false, plugin.StatusRolledBack, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.tg.Name, "version": previous.Version}
		if a.slot != "" {
			return d, a.swapBack(ctx, previous.Version)
		}
		tags, err := a.tags(ctx)
		if err != nil {
			return d, err
		}
		if tags[TagPending] != req.DeploymentID && tags[TagDeployment] != req.DeploymentID {
			return d, nil
		}
		image := previous.Details[DetailImage+"@"+a.host]
		if image == "" {
			return d, errors.New("without a slot only a container image can be rolled back: configure a slot (parameter slot) to roll back packages")
		}
		cfg, err := a.webConfig(ctx, "")
		if err != nil {
			return d, err
		}
		if runningImage(cfg) != image {
			if err := a.patchWebConfig(ctx, "", Object{fxField(cfg): "DOCKER|" + image}); err != nil {
				return d, err
			}
		}
		a.log.WarnContext(ctx, "immagine precedente ripristinata", "version", previous.Version, "image", image, "rollback", true)
		return d, a.setTags(ctx, map[string]string{TagVersion: previous.Version, TagDeployment: "", TagImage: image, TagPending: ""})
	})
}

// swapBack swaps again if production runs this deploy and the slot holds the previous version.
func (a *app) swapBack(ctx context.Context, version string) error {
	prod, err := a.appSettings(ctx, "")
	if err != nil {
		return err
	}
	if prod[SettingDeployment] != a.req.DeploymentID {
		// Production was not touched: only the slot has the new version.
		return a.setRamp(ctx, 0)
	}
	slot, err := a.appSettings(ctx, a.slot)
	if err != nil {
		return err
	}
	if version != "" && slot[SettingVersion] != version {
		return fmt.Errorf("slot %s holds version %q, not %s: swap back impossible", a.slot, slot[SettingVersion], version)
	}
	if err := a.setRamp(ctx, 0); err != nil {
		return err
	}
	if err := a.swap(ctx); err != nil {
		return err
	}
	a.log.WarnContext(ctx, "release precedente di nuovo in produzione (swap)", "version", version, "rollback", true)
	return nil
}

// DryRun implements plugin.DryRunner: it reads web app, slot, configuration and settings and,
// for a package, the Kudu deployments; it changes nothing.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.each(ctx, req, false, plugin.StatusReady, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.tg.Name}
		o, err := a.get(ctx, "")
		if err != nil {
			return d, err
		}
		d["state"] = str(o, "properties", "state")
		target := ""
		if a.slot != "" {
			if _, err := a.get(ctx, a.slot); err != nil {
				return d, err
			}
			target, d["slot"] = a.slot, a.slot
		}
		v, _, err := a.current(ctx)
		if err != nil {
			return d, err
		}
		d["current"] = v
		if !isImage(a.req) && a.req.Artifact.LocalPath != "" {
			base, err := a.kudu(ctx, target)
			if err != nil {
				return d, err
			}
			if _, err := a.c.Raw(ctx, http.MethodGet, base+"/api/deployments/latest", nil, "", nil); err != nil && !errors.Is(err, azure.ErrNotFound) {
				return d, fmt.Errorf("Kudu of %s: %w", where(target), err)
			}
		}
		return d, nil
	})
}

// Percentage rollouts: the slot is the candidate, production the stable version; the
// rampUpRules of production route a share of the requests to the slot, the promotion is the
// swap. They require a slot.

func requireSlot(req *plugin.Request) error {
	if req.Params[ParamSlot] == "" {
		return errors.New("strategy: traffic with the appservice plugin requires a deployment slot (parameter slot)")
	}
	return nil
}

// PrepareSlots implements plugin.SlotDeployer: it records the version in production.
func (p *Plugin) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if err := requireSlot(req); err != nil {
		return nil, err
	}
	state := map[string]string{plugin.StateHasStable: "true"}
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		v, _, err := a.current(ctx)
		if err != nil {
			return nil, fmt.Errorf("web app %s: %w", t.Host, err)
		}
		state["stable@"+t.Host] = v
	}
	return state, nil
}

// DeployCandidate implements plugin.SlotDeployer: the release in the slot, without traffic.
func (p *Plugin) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	if err := requireSlot(req); err != nil {
		return nil, err
	}
	return p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		d := map[string]string{"app": a.tg.Name, "slot": a.slot, "version": req.Artifact.Version}
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return d, err
		}
		if prod[SettingDeployment] == req.DeploymentID {
			return d, nil
		}
		if err := a.setRamp(ctx, 0); err != nil {
			return d, err
		}
		return d, a.releaseToSlot(ctx)
	})
}

// rampRules reads the rampUpRules of production.
func (a *app) rampRules(ctx context.Context) ([]any, error) {
	cfg, err := a.webConfig(ctx, "")
	if err != nil {
		return nil, err
	}
	rules, _ := get(cfg, "properties", "experiments", "rampUpRules").([]any)
	return rules, nil
}

// setRamp routes percent% of production traffic to the slot (0 removes the rule).
func (a *app) setRamp(ctx context.Context, percent int) error {
	rules, err := a.rampRules(ctx)
	if err != nil {
		return err
	}
	host, err := a.hostName(ctx, a.slot)
	if err != nil {
		return err
	}
	var out []any
	cur := -1.0
	for _, r := range rules {
		m, _ := r.(map[string]any)
		if n, _ := m["name"].(string); n == a.slot {
			cur, _ = m["reroutePercentage"].(float64)
			continue
		}
		out = append(out, r)
	}
	if (percent == 0 && cur < 0) || float64(percent) == cur {
		return nil
	}
	if percent > 0 {
		out = append(out, Object{"name": a.slot, "actionHostName": host, "reroutePercentage": percent})
	}
	if out == nil {
		out = []any{}
	}
	return a.patchWebConfig(ctx, "", Object{"experiments": Object{"rampUpRules": out}})
}

// SetCandidateWeight implements plugin.TrafficRouter.
func (p *Plugin) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	_, err := p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return nil, err
		}
		if prod[SettingDeployment] == req.DeploymentID {
			return nil, nil
		}
		if err := a.setRamp(ctx, percent); err != nil {
			return nil, err
		}
		a.log.InfoContext(ctx, "quota di traffico verso lo slot aggiornata", "percent", percent)
		return nil, nil
	})
	return err
}

// CandidateWeight implements plugin.TrafficRouter: 100 once swapped.
func (p *Plugin) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	weight := -2
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return 0, err
		}
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return 0, fmt.Errorf("web app %s: %w", t.Host, err)
		}
		w := 0
		if prod[SettingDeployment] == req.DeploymentID {
			w = 100
		} else {
			rules, err := a.rampRules(ctx)
			if err != nil {
				return 0, fmt.Errorf("web app %s: %w", t.Host, err)
			}
			for _, r := range rules {
				m, _ := r.(map[string]any)
				if n, _ := m["name"].(string); n == a.slot {
					f, _ := m["reroutePercentage"].(float64)
					w = int(math.Round(f))
				}
			}
		}
		if weight != -2 && w != weight {
			return -1, nil
		}
		weight = w
	}
	return max(weight, 0), nil
}

// RouteTesters implements plugin.TesterRouter: testers use the slot's own host name (or the
// x-ms-routing-name=<slot> cookie on production).
func (p *Plugin) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	if err := requireSlot(req); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range req.Targets {
		a, err := p.open(ctx, req, t)
		if err != nil {
			return nil, err
		}
		u, err := a.siteURL(ctx, a.slot, "/")
		if err != nil {
			return nil, fmt.Errorf("web app %s: %w", t.Host, err)
		}
		out["tester_url@"+t.Host] = u
	}
	out["tester_cookie"] = "x-ms-routing-name=" + req.Params[ParamSlot]
	return out, nil
}

// PromoteCandidate implements plugin.SlotDeployer: the routing rule is removed and the slot
// swapped with production.
func (p *Plugin) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	_, err := p.each(ctx, req, true, plugin.StatusDeployed, func(ctx context.Context, a *app) (map[string]string, error) {
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return nil, err
		}
		if err := a.setRamp(ctx, 0); err != nil {
			return nil, err
		}
		if prod[SettingDeployment] == req.DeploymentID {
			return nil, nil
		}
		return nil, a.swap(ctx)
	})
	return err
}

// RevertCandidate implements plugin.SlotDeployer: no traffic to the slot and, if already
// swapped, the stable version back in production.
func (p *Plugin) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	_, err := p.each(ctx, req, false, plugin.StatusRolledBack, func(ctx context.Context, a *app) (map[string]string, error) {
		stable := ""
		if req.Traffic != nil {
			stable = req.Traffic.State["stable@"+a.host]
		}
		prod, err := a.appSettings(ctx, "")
		if err != nil {
			return nil, err
		}
		if prod[SettingDeployment] != req.DeploymentID {
			return nil, a.setRamp(ctx, 0)
		}
		return nil, a.swapBack(ctx, stable)
	})
	return err
}

// RetirePrevious implements plugin.SlotDeployer: the replaced version stays in the slot, as
// the point to swap back to.
func (p *Plugin) RetirePrevious(context.Context, *plugin.Request) error { return nil }

var (
	_ plugin.DryRunner        = (*Plugin)(nil)
	_ plugin.ReleaseInspector = (*Plugin)(nil)
	_ plugin.Rollbacker       = (*Plugin)(nil)
	_ plugin.SlotDeployer     = (*Plugin)(nil)
	_ plugin.TrafficRouter    = (*Plugin)(nil)
	_ plugin.TesterRouter     = (*Plugin)(nil)
)
