package traffic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Settings of the haproxy provider (Data Plane API).
const (
	settingDataplaneURLs = "dataplane_urls"     // Data Plane API URL of every active instance, comma separated
	settingBackend       = "backend"            // HAProxy backend with the members
	settingCredentials   = "credentials_secret" // "user:password" secret of the Data Plane API
	settingAPIVersion    = "api_version"        // v3 (default) or v2
)

// haproxyPool drains the servers of a backend with the Data Plane API of every HAProxy
// instance: a member is drained only when it is drained on all of them.
type haproxyPool struct {
	urls    []string
	backend string
	version string
	creds   string
	client  *http.Client
}

func validateHAProxy(s map[string]string) error {
	var errs []error
	urls := splitList(s[settingDataplaneURLs])
	if len(urls) == 0 {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingDataplaneURLs))
	}
	for _, u := range urls {
		if err := checkURL(s, settingDataplaneURLs, u); err != nil {
			errs = append(errs, err)
		}
	}
	if s[settingBackend] == "" {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingBackend))
	}
	if v := s[settingAPIVersion]; v != "" && v != "v2" && v != "v3" {
		errs = append(errs, fmt.Errorf("%s %q non valido (v2|v3)", settingAPIVersion, v))
	}
	return errors.Join(errs...)
}

// NewHAProxyPool builds the haproxy pool.
func NewHAProxyPool(ctx context.Context, env Env) (Pool, error) {
	if err := validateHAProxy(env.Settings); err != nil {
		return nil, err
	}
	c, err := httpClient(env.Settings, env.Timeout)
	if err != nil {
		return nil, err
	}
	p := &haproxyPool{urls: splitList(env.Settings[settingDataplaneURLs]), backend: env.Settings[settingBackend], version: env.Settings[settingAPIVersion], client: c}
	if p.version == "" {
		p.version = "v3"
	}
	if name := env.Settings[settingCredentials]; name != "" {
		if p.creds, err = env.Secrets.Secret(ctx, name); err != nil {
			return nil, fmt.Errorf("%s: %w", settingCredentials, err)
		}
	}
	return p, nil
}

// haproxyServer is a backend server in its runtime state.
type haproxyServer struct {
	Name             string `json:"name"`
	AdminState       string `json:"admin_state"`
	OperationalState string `json:"operational_state"`
}

func (p *haproxyPool) serversPath(name string) string {
	if p.version == "v2" {
		path := "/v2/services/haproxy/runtime/servers"
		if name != "" {
			path += "/" + url.PathEscape(name)
		}
		return path + "?backend=" + url.QueryEscape(p.backend)
	}
	path := "/v3/services/haproxy/runtime/backends/" + url.PathEscape(p.backend) + "/servers"
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path
}

// Members implements Pool: a member is active and healthy only if it is on all instances.
func (p *haproxyPool) Members(ctx context.Context) ([]Member, error) {
	byName := map[string]*Member{}
	var order []string
	for _, base := range p.urls {
		var list []haproxyServer
		if err := p.do(ctx, http.MethodGet, base+p.serversPath(""), nil, &list); err != nil {
			return nil, err
		}
		for _, s := range list {
			m := byName[s.Name]
			if m == nil {
				m = &Member{Name: s.Name, State: StateActive, Healthy: true}
				byName[s.Name] = m
				order = append(order, s.Name)
			}
			switch s.AdminState {
			case "drain":
				if m.State == StateActive {
					m.State = StateDraining
				}
			case "maint":
				m.State = StateDisabled
			}
			m.Healthy = m.Healthy && s.OperationalState == "up"
		}
	}
	out := make([]Member, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

func (p *haproxyPool) setState(ctx context.Context, members []string, state string) error {
	for _, base := range p.urls {
		for _, m := range members {
			body, _ := json.Marshal(map[string]string{"admin_state": state})
			if err := p.do(ctx, http.MethodPut, base+p.serversPath(m), body, nil); err != nil {
				return fmt.Errorf("%s: server %s: %w", base, m, err)
			}
		}
	}
	return nil
}

// Drain implements Pool.
func (p *haproxyPool) Drain(ctx context.Context, members []string) error {
	return p.setState(ctx, members, "drain")
}

// Restore implements Pool.
func (p *haproxyPool) Restore(ctx context.Context, members []string) error {
	return p.setState(ctx, members, "ready")
}

// Connections implements Pool: it sums the current sessions (scur) of all instances.
func (p *haproxyPool) Connections(ctx context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, base := range p.urls {
		for _, m := range members {
			q := url.Values{"type": {"server"}, "parent": {p.backend}, "name": {m}}
			var raw json.RawMessage
			if err := p.do(ctx, http.MethodGet, base+"/"+p.version+"/services/haproxy/stats/native?"+q.Encode(), nil, &raw); err != nil {
				return nil, err
			}
			n, err := haproxySessions(raw, m)
			if err != nil {
				return nil, fmt.Errorf("%s: statistiche del server %s: %w", base, m, err)
			}
			out[m] += n
		}
	}
	return out, nil
}

// haproxySessions reads scur from the native stats: an object (v3) or a list of objects,
// one per process (v2).
func haproxySessions(raw json.RawMessage, name string) (int, error) {
	type entry struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Stats struct {
			Scur *int `json:"scur"`
		} `json:"stats"`
	}
	type doc struct {
		Stats []entry `json:"stats"`
	}
	var docs []doc
	if err := json.Unmarshal(raw, &docs); err != nil {
		var one doc
		if err := json.Unmarshal(raw, &one); err != nil {
			return 0, err
		}
		docs = []doc{one}
	}
	total, found := 0, false
	for _, d := range docs {
		for _, e := range d.Stats {
			if e.Name == name && (e.Type == "" || e.Type == "server") && e.Stats.Scur != nil {
				total += *e.Stats.Scur
				found = true
			}
		}
	}
	if !found {
		return 0, errors.New("server assente dalle statistiche")
	}
	return total, nil
}

// Healthy implements Pool.
func (p *haproxyPool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	all, err := p.Members(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range members {
		i := slices.IndexFunc(all, func(x Member) bool { return x.Name == m })
		out[m] = i >= 0 && all[i].InService()
	}
	return out, nil
}

func (p *haproxyPool) do(ctx context.Context, method, u string, body []byte, out any) error {
	return doJSON(ctx, p.client, method, u, body, out, func(r *http.Request) {
		if user, pass, ok := strings.Cut(p.creds, ":"); ok {
			r.SetBasicAuth(user, strings.TrimSpace(pass))
		}
	})
}

// doJSON runs a JSON request: a status outside 2xx is an error with an excerpt of the
// response, never the credentials.
func doJSON(ctx context.Context, c *http.Client, method, u string, body []byte, out any, auth func(*http.Request)) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if auth != nil {
		auth(req)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &StatusError{Method: method, Path: redactURL(u), Status: resp.StatusCode, Body: msg}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// StatusError is a failed HTTP response of a management API.
type StatusError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: stato %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// redactURL strips credentials and query from a URL for messages.
func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	p.User, p.RawQuery = nil, ""
	return p.String()
}
