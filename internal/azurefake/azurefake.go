// Package azurefake emulates, at protocol level, the Azure services used by manent's plugins:
// the Microsoft Entra ID token, the Resource Manager API of App Service (web apps, slots, site
// configuration, app settings, tags, swap) and of Container Apps (apps, revisions, traffic),
// Kudu (OneDeploy and deployment records), the web apps and container apps themselves (they
// serve the released files or the version of the image) and the Log Analytics query API.
//
// It is used by the plugin tests and by the use cases (test/usecases/fakes). Long-running
// operations answer 202 with Location or Azure-AsyncOperation, completed at the second read.
package azurefake

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Object is a JSON object.
type Object = map[string]any

// Site is a web app or a slot.
type Site struct {
	ResourceGroup, Name, Slot string
	Settings, Tags            map[string]string
	// LinuxFx is linuxFxVersion ("DOCKER|<image>" for a container).
	LinuxFx string
	// Files are the files of the last package (OneDeploy), by path.
	Files       map[string]string
	Ramp        []Object
	Deployments []Object
	Restarts    int
}

// Host is the default host name of the site.
func (s *Site) Host() string {
	n := s.Name
	if s.Slot != "" {
		n += "-" + s.Slot
	}
	return strings.ToLower(n) + ".azurewebsites.net"
}

// SCM is the Kudu host name of the site.
func (s *Site) SCM() string {
	return strings.TrimSuffix(s.Host(), ".azurewebsites.net") + ".scm.azurewebsites.net"
}

// Revision is a revision of a container app.
type Revision struct {
	Name     string
	Template Object
	Active   bool
	Healthy  bool
	Created  time.Time
}

// App is a container app.
type App struct {
	ResourceGroup, Name string
	// Mode is the activeRevisionsMode: Single or Multiple.
	Mode        string
	Domain      string
	Template    Object
	Traffic     []any
	Revisions   map[string]*Revision
	Latest      string
	LatestReady string
	Patches     int
}

// FQDN is the host name of the app's ingress.
func (a *App) FQDN() string { return a.Name + "." + a.Domain }

// Fake is the emulated Azure. Fields are read and written under Lock.
type Fake struct {
	mu sync.Mutex
	// ClientSecret is the accepted client secret of the service principal (any client ID).
	ClientSecret string
	Sites        map[string]*Site // key: "<name>" or "<name>/<slot>", lowercase
	Apps         map[string]*App  // key: name, lowercase
	// Series are the Log Analytics results: the first entry whose Match is in the query wins.
	Series []Series
	// Calls records "<METHOD> <path>" of every request (for the tests).
	Calls []string
	// Queries records the Log Analytics queries with their timespan.
	Queries []string
	ops     map[string]int
	seq     int
}

// Series is a Log Analytics result: one row per value.
type Series struct {
	Match  string    `json:"match"`
	Values []float64 `json:"values"`
}

// New creates an empty fake.
func New(secret string) *Fake {
	return &Fake{ClientSecret: secret, Sites: map[string]*Site{}, Apps: map[string]*App{}, ops: map[string]int{}}
}

// Lock and Unlock guard the state read or changed by tests and use cases.
func (f *Fake) Lock()   { f.mu.Lock() }
func (f *Fake) Unlock() { f.mu.Unlock() }

// AddSite adds a web app (slot "") or a slot.
func (f *Fake) AddSite(rg, name, slot string) *Site {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &Site{ResourceGroup: rg, Name: name, Slot: slot, Settings: map[string]string{}, Tags: map[string]string{}, Files: map[string]string{}}
	f.Sites[siteKey(name, slot)] = s
	return s
}

// AddApp adds a container app with one revision of the given image, all traffic on it.
func (f *Fake) AddApp(rg, name, mode, container, image string) *App {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &App{ResourceGroup: rg, Name: name, Mode: mode, Domain: "nicebeach-1a2b3c.westeurope.azurecontainerapps.io", Revisions: map[string]*Revision{}}
	a.Template = Object{"containers": []any{Object{"name": container, "image": image}}, "scale": Object{"minReplicas": 1}}
	f.newRevision(a, "initial")
	a.Traffic = []any{Object{"latestRevision": true, "weight": 100}}
	f.Apps[strings.ToLower(name)] = a
	return a
}

func siteKey(name, slot string) string {
	k := strings.ToLower(name)
	if slot != "" {
		k += "/" + slot
	}
	return k
}

// Handler serves the emulated APIs.
func (f *Fake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Calls = append(f.Calls, r.Method+" "+r.URL.Path)
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/oauth2/v2.0/token"):
			f.token(w, r)
		case strings.HasPrefix(p, "/fake/"):
			f.admin(w, r)
		case strings.HasPrefix(p, "/operations/"):
			f.operation(w, r)
		case strings.HasPrefix(p, "/scm/"):
			if f.auth(w, r) {
				f.kudu(w, r)
			}
		case strings.HasPrefix(p, "/site/"):
			f.serveSite(w, r)
		case strings.HasPrefix(p, "/app/"):
			f.serveApp(w, r)
		case strings.HasPrefix(p, "/v1/workspaces/"):
			if f.auth(w, r) {
				f.logs(w, r)
			}
		case strings.HasPrefix(p, "/subscriptions/"):
			if f.auth(w, r) {
				f.arm(w, r)
			}
		default:
			reply(w, 404, Object{"error": Object{"code": "NotFound", "message": "unknown path " + p}})
		}
	})
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func armError(w http.ResponseWriter, code int, c, msg string) {
	reply(w, code, Object{"error": Object{"code": c, "message": msg}})
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != f.ClientSecret || r.Form.Get("client_id") == "" {
		reply(w, 401, Object{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided."})
		return
	}
	reply(w, 200, Object{"token_type": "Bearer", "expires_in": 3599, "access_token": "fake-token-" + r.Form.Get("scope")})
}

func (f *Fake) auth(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer fake-token-") {
		armError(w, 401, "AuthenticationFailed", "missing or invalid bearer token")
		return false
	}
	return true
}

// admin: GET /fake/state (the state as JSON), POST /fake/series (adds a Log Analytics result).
func (f *Fake) admin(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/fake/state":
		apps := Object{}
		for k, a := range f.Apps {
			revs := Object{}
			for n, rv := range a.Revisions {
				revs[n] = Object{"active": rv.Active, "healthy": rv.Healthy, "image": image(rv.Template)}
			}
			apps[k] = Object{"traffic": a.Traffic, "latest": a.Latest, "latestReady": a.LatestReady, "revisions": revs, "patches": a.Patches}
		}
		sites := Object{}
		for k, s := range f.Sites {
			sites[k] = Object{"settings": s.Settings, "tags": s.Tags, "linuxFx": s.LinuxFx, "version": s.Files["version"], "ramp": s.Ramp,
				"deployments": len(s.Deployments), "restarts": s.Restarts}
		}
		reply(w, 200, Object{"sites": sites, "apps": apps, "queries": f.Queries})
	case "/fake/series":
		var s Series
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			reply(w, 400, Object{"error": err.Error()})
			return
		}
		f.Series = append([]Series{s}, f.Series...)
		reply(w, 200, Object{})
	default:
		reply(w, 404, nil)
	}
}

// accepted answers 202 for a long-running operation, completed at the second read.
func (f *Fake) accepted(w http.ResponseWriter, r *http.Request, async bool) {
	f.seq++
	id := fmt.Sprintf("op-%d", f.seq)
	u := "http://" + r.Host + "/operations/" + id
	if async {
		w.Header().Set("Azure-AsyncOperation", u)
	} else {
		w.Header().Set("Location", u)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *Fake) operation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/operations/")
	f.ops[id]++
	// The first read finds the operation in progress (202 for Location, InProgress for
	// Azure-AsyncOperation), the second completed.
	if f.ops[id] < 2 {
		reply(w, 202, Object{"status": "InProgress"})
		return
	}
	reply(w, 200, Object{"status": "Succeeded"})
}

var (
	siteRe = regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/([^/]+)/providers/Microsoft\.Web/sites/([^/]+)(?:/slots/([^/]+))?(/.*)?$`)
	appRe  = regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/([^/]+)/providers/Microsoft\.App/containerApps/([^/]+)(/.*)?$`)
)

func (f *Fake) arm(w http.ResponseWriter, r *http.Request) {
	if m := siteRe.FindStringSubmatch(r.URL.Path); m != nil {
		s, ok := f.Sites[siteKey(m[2], m[3])]
		if !ok || !strings.EqualFold(s.ResourceGroup, m[1]) {
			armError(w, 404, "ResourceNotFound", "The Resource 'Microsoft.Web/sites/"+m[2]+"' under resource group '"+m[1]+"' was not found.")
			return
		}
		f.site(w, r, s, m[4])
		return
	}
	if m := appRe.FindStringSubmatch(r.URL.Path); m != nil {
		a, ok := f.Apps[strings.ToLower(m[2])]
		if !ok || !strings.EqualFold(a.ResourceGroup, m[1]) {
			armError(w, 404, "ResourceNotFound", "The Resource 'Microsoft.App/containerApps/"+m[2]+"' under resource group '"+m[1]+"' was not found.")
			return
		}
		f.app(w, r, a, m[3])
		return
	}
	armError(w, 404, "NotFound", "unknown resource "+r.URL.Path)
}

func decode(r *http.Request) Object {
	var o Object
	_ = json.NewDecoder(r.Body).Decode(&o)
	return o
}

func (s *Site) json() Object {
	tags := Object{}
	for k, v := range s.Tags {
		tags[k] = v
	}
	name := s.Name
	if s.Slot != "" {
		name += "/" + s.Slot
	}
	return Object{"name": name, "tags": tags, "properties": Object{"state": "Running", "defaultHostName": s.Host(),
		"enabledHostNames": []any{s.Host(), s.SCM()}}}
}

func (f *Fake) site(w http.ResponseWriter, r *http.Request, s *Site, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		reply(w, 200, s.json())
	case sub == "/config/web" && r.Method == http.MethodGet:
		rules := s.Ramp
		if rules == nil {
			rules = []Object{}
		}
		reply(w, 200, Object{"properties": Object{"linuxFxVersion": s.LinuxFx, "experiments": Object{"rampUpRules": rules}}})
	case sub == "/config/web" && r.Method == http.MethodPatch:
		props, _ := decode(r)["properties"].(map[string]any)
		if v, ok := props["linuxFxVersion"].(string); ok && v != s.LinuxFx {
			s.LinuxFx = v
			s.Restarts++
		}
		if ex, ok := props["experiments"].(map[string]any); ok {
			s.Ramp = nil
			rules, _ := ex["rampUpRules"].([]any)
			for _, x := range rules {
				if m, ok := x.(map[string]any); ok {
					s.Ramp = append(s.Ramp, m)
				}
			}
		}
		reply(w, 200, Object{"properties": props})
	case sub == "/config/appsettings/list" && r.Method == http.MethodPost:
		reply(w, 200, Object{"properties": s.Settings})
	case sub == "/config/appsettings" && r.Method == http.MethodPut:
		props, _ := decode(r)["properties"].(map[string]any)
		s.Settings = map[string]string{}
		for k, v := range props {
			s.Settings[k] = fmt.Sprint(v)
		}
		s.Restarts++
		reply(w, 200, Object{"properties": s.Settings})
	case sub == "/providers/Microsoft.Resources/tags/default" && r.Method == http.MethodPatch:
		body := decode(r)
		props, _ := body["properties"].(map[string]any)
		tags, _ := props["tags"].(map[string]any)
		for k, v := range tags {
			if body["operation"] == "Delete" {
				delete(s.Tags, k)
			} else {
				s.Tags[k] = fmt.Sprint(v)
			}
		}
		reply(w, 200, Object{"properties": Object{"tags": s.Tags}})
	case sub == "/slotsswap" && r.Method == http.MethodPost:
		if s.Slot == "" || decode(r)["targetSlot"] != "production" {
			armError(w, 400, "BadRequest", "only slot to production swaps are emulated")
			return
		}
		prod := f.Sites[siteKey(s.Name, "")]
		// The swap exchanges content and the settings that are not sticky (all, here).
		prod.Settings, s.Settings = s.Settings, prod.Settings
		prod.LinuxFx, s.LinuxFx = s.LinuxFx, prod.LinuxFx
		prod.Files, s.Files = s.Files, prod.Files
		prod.Deployments, s.Deployments = s.Deployments, prod.Deployments
		f.accepted(w, r, false)
	default:
		armError(w, 405, "MethodNotAllowed", r.Method+" "+sub+" not emulated")
	}
}

// kudu serves /scm/<scm host>/api/...: OneDeploy (a package with a "fail" file fails) and the
// deployment records.
func (f *Fake) kudu(w http.ResponseWriter, r *http.Request) {
	host, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/scm/"), "/")
	var s *Site
	for _, x := range f.Sites {
		if x.SCM() == host {
			s = x
		}
	}
	if s == nil {
		reply(w, 404, nil)
		return
	}
	switch {
	case rest == "api/deployments/latest" && r.Method == http.MethodGet:
		if len(s.Deployments) == 0 {
			reply(w, 404, Object{"Message": "no deployments"})
			return
		}
		reply(w, 200, s.Deployments[len(s.Deployments)-1])
	case rest == "api/publish" && r.Method == http.MethodPost:
		data, _ := io.ReadAll(r.Body)
		files, err := unzip(data)
		f.seq++
		d := Object{"id": fmt.Sprintf("dep-%d", f.seq), "complete": true, "status": 4, "status_text": "", "deployer": "OneDeploy",
			"log_url": "https://" + host + "/api/deployments/latest/log", "type": r.URL.Query().Get("type")}
		if err != nil || files["fail"] != "" {
			d["status"], d["status_text"] = 3, "Deployment failed"
		} else {
			s.Files = files
			s.Restarts++
		}
		s.Deployments = append(s.Deployments, d)
		w.WriteHeader(http.StatusAccepted)
	default:
		reply(w, 404, nil)
	}
}

func unzip(data []byte) (map[string]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		out[strings.TrimPrefix(zf.Name, "./")] = string(b)
	}
	return out, nil
}

// imageVersion is the tag of an image reference (repository:tag@sha256:...).
func imageVersion(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[i+1:]
	}
	return ""
}

// serveSite serves /site/<host>/<path>: the package files, or for a container "version" (the
// image tag) and "healthz" (200 unless the image is "broken"). On production, a request with
// the x-ms-routing-name cookie (or query) goes to that slot.
func (f *Fake) serveSite(w http.ResponseWriter, r *http.Request) {
	host, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/site/"), "/")
	var s *Site
	for _, x := range f.Sites {
		if x.Host() == host {
			s = x
		}
	}
	if s == nil {
		http.NotFound(w, r)
		return
	}
	slot := r.URL.Query().Get("x-ms-routing-name")
	if c, err := r.Cookie("x-ms-routing-name"); err == nil {
		slot = c.Value
	}
	if s.Slot == "" && slot != "" && slot != "self" {
		if x, ok := f.Sites[siteKey(s.Name, slot)]; ok {
			s = x
		}
	}
	if img, ok := strings.CutPrefix(s.LinuxFx, "DOCKER|"); ok {
		switch path {
		case "version":
			_, _ = io.WriteString(w, imageVersion(img)+"\n")
		case "healthz":
			if strings.Contains(img, "broken") {
				http.Error(w, "unhealthy", 503)
				return
			}
			_, _ = io.WriteString(w, "ok\n")
		default:
			http.NotFound(w, r)
		}
		return
	}
	if c, ok := s.Files[path]; ok {
		_, _ = io.WriteString(w, c)
		return
	}
	http.NotFound(w, r)
}

func image(tmpl Object) string {
	cs, _ := tmpl["containers"].([]any)
	if len(cs) == 0 {
		return ""
	}
	c, _ := cs[0].(map[string]any)
	s, _ := c["image"].(string)
	return s
}

// newRevision creates the revision of the app's current template.
func (f *Fake) newRevision(a *App, suffix string) *Revision {
	f.seq++
	if suffix == "" {
		suffix = fmt.Sprintf("r%07d", f.seq)
	}
	b, _ := json.Marshal(a.Template)
	var tmpl Object
	_ = json.Unmarshal(b, &tmpl)
	rv := &Revision{Name: a.Name + "--" + suffix, Template: tmpl, Active: true, Healthy: !strings.Contains(image(tmpl), "broken"),
		Created: time.Now().Add(time.Duration(f.seq) * time.Millisecond)}
	a.Revisions[rv.Name] = rv
	a.Latest = rv.Name
	if rv.Healthy {
		a.LatestReady = rv.Name
		if a.Mode != "Multiple" {
			for _, o := range a.Revisions {
				o.Active = o == rv
			}
		}
	}
	return rv
}

func (a *App) json() Object {
	return Object{"name": a.Name, "properties": Object{"provisioningState": "Succeeded", "latestRevisionName": a.Latest,
		"latestReadyRevisionName": a.LatestReady, "configuration": Object{"activeRevisionsMode": a.Mode,
			"ingress": Object{"external": true, "targetPort": 8080, "fqdn": a.FQDN(), "traffic": a.Traffic}},
		"template": a.Template}}
}

func (rv *Revision) json() Object {
	running, health := "Running", "Healthy"
	if !rv.Healthy {
		running, health = "Failed", "Unhealthy"
	}
	if !rv.Active {
		running = "Stopped"
	}
	return Object{"name": rv.Name, "properties": Object{"active": rv.Active, "provisioningState": "Provisioned", "runningState": running,
		"healthState": health, "createdTime": rv.Created.UTC().Format(time.RFC3339Nano), "template": rv.Template,
		"runningStateDetails": map[bool]string{true: "", false: "Container 'app' crashed: exit code 1"}[rv.Healthy]}}
}

func (f *Fake) app(w http.ResponseWriter, r *http.Request, a *App, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		reply(w, 200, a.json())
	case sub == "" && r.Method == http.MethodPatch:
		props, _ := decode(r)["properties"].(map[string]any)
		a.Patches++
		if tmpl, ok := props["template"].(map[string]any); ok {
			suffix, _ := tmpl["revisionSuffix"].(string)
			if _, exists := a.Revisions[a.Name+"--"+suffix]; suffix != "" && exists {
				armError(w, 400, "ContainerAppRevisionSuffixConflict", "revision with suffix "+suffix+" already exists")
				return
			}
			a.Template = tmpl
			f.newRevision(a, suffix)
		}
		if cfg, ok := props["configuration"].(map[string]any); ok {
			if in, ok := cfg["ingress"].(map[string]any); ok {
				if tr, ok := in["traffic"].([]any); ok {
					for _, t := range tr {
						m, _ := t.(map[string]any)
						if n, _ := m["revisionName"].(string); n != "" {
							if rv, ok := a.Revisions[n]; !ok || !rv.Active {
								armError(w, 400, "ContainerAppInvalidTrafficWeight", "revision "+n+" not found or not active")
								return
							}
						}
					}
					a.Traffic = tr
				}
			}
		}
		w.Header().Set("Azure-AsyncOperation", "http://"+r.Host+"/operations/async-"+fmt.Sprint(a.Patches))
		reply(w, 202, nil)
	case sub == "/revisions" && r.Method == http.MethodGet:
		var list []any
		names := make([]string, 0, len(a.Revisions))
		for n := range a.Revisions {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			list = append(list, a.Revisions[n].json())
		}
		reply(w, 200, Object{"value": list})
	case strings.HasPrefix(sub, "/revisions/"):
		name, action, _ := strings.Cut(strings.TrimPrefix(sub, "/revisions/"), "/")
		rv, ok := a.Revisions[name]
		if !ok {
			armError(w, 404, "ResourceNotFound", "revision "+name+" not found")
			return
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			reply(w, 200, rv.json())
		case action == "activate" && r.Method == http.MethodPost:
			rv.Active = true
			reply(w, 200, nil)
		case action == "deactivate" && r.Method == http.MethodPost:
			rv.Active = false
			reply(w, 200, nil)
		default:
			armError(w, 405, "MethodNotAllowed", r.Method+" "+sub)
		}
	default:
		armError(w, 405, "MethodNotAllowed", r.Method+" "+sub+" not emulated")
	}
}

// serving is the revision that receives a request: the one with the largest weight, or the
// one with the label (host <app>---<label>.<domain>).
func (a *App) serving(label string) *Revision {
	best, w := "", -1.0
	for _, t := range a.Traffic {
		m, _ := t.(map[string]any)
		name, _ := m["revisionName"].(string)
		if m["latestRevision"] == true {
			name = a.LatestReady
		}
		if label != "" {
			if m["label"] == label {
				return a.Revisions[name]
			}
			continue
		}
		if wt, _ := m["weight"].(float64); wt > w {
			best, w = name, wt
		}
	}
	return a.Revisions[best]
}

// serveApp serves /app/<fqdn>/<path>: "version" is the version of the serving revision,
// "healthz" 200 unless the image is broken.
func (f *Fake) serveApp(w http.ResponseWriter, r *http.Request) {
	host, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/app/"), "/")
	name, _, _ := strings.Cut(host, ".")
	label := ""
	if n, l, ok := strings.Cut(name, "---"); ok {
		name, label = n, l
	}
	a, ok := f.Apps[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	rv := a.serving(label)
	if rv == nil {
		http.Error(w, "no revision", 404)
		return
	}
	switch path {
	case "version":
		// The MANENT_VERSION variable of the revision, or the image tag.
		v := imageVersion(image(rv.Template))
		cs, _ := rv.Template["containers"].([]any)
		if len(cs) > 0 {
			c, _ := cs[0].(map[string]any)
			env, _ := c["env"].([]any)
			for _, e := range env {
				if m, _ := e.(map[string]any); m["name"] == "MANENT_VERSION" {
					v, _ = m["value"].(string)
				}
			}
		}
		_, _ = io.WriteString(w, v+"\n")
	case "healthz":
		if !rv.Healthy {
			http.Error(w, "unhealthy", 503)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	default:
		http.NotFound(w, r)
	}
}

// logs answers the Log Analytics queries with the configured series.
func (f *Fake) logs(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Query    string `json:"query"`
		Timespan string `json:"timespan"`
	}
	_ = json.NewDecoder(r.Body).Decode(&q)
	f.Queries = append(f.Queries, q.Query+" | "+q.Timespan)
	rows := []any{}
	for _, s := range f.Series {
		if strings.Contains(q.Query, s.Match) {
			for i, v := range s.Values {
				rows = append(rows, []any{time.Unix(int64(i)*60, 0).UTC().Format(time.RFC3339), v})
			}
			break
		}
	}
	reply(w, 200, Object{"tables": []any{Object{"name": "PrimaryResult",
		"columns": []any{Object{"name": "TimeGenerated", "type": "datetime"}, Object{"name": "value", "type": "real"}}, "rows": rows}}})
}
