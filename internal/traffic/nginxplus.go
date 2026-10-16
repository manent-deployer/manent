package traffic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Settings of the nginx-plus provider (NGINX Plus API).
const (
	settingAPIURLs  = "api_urls"    // API URL of every instance (e.g. https://lb1:8080/api), comma separated
	settingUpstream = "upstream"    // http upstream with the servers
	settingAPIVer   = "api_version" // API version (default 9)
)

var upstreamRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// nginxPlusPool drains the servers of an upstream with the NGINX Plus API on every instance.
type nginxPlusPool struct {
	urls     []string
	upstream string
	version  string
	creds    string
	client   *http.Client
}

func validateNginxPlus(s map[string]string) error {
	var errs []error
	urls := splitList(s[settingAPIURLs])
	if len(urls) == 0 {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingAPIURLs))
	}
	for _, u := range urls {
		if err := checkURL(s, settingAPIURLs, u); err != nil {
			errs = append(errs, err)
		}
	}
	if !upstreamRe.MatchString(s[settingUpstream]) {
		errs = append(errs, fmt.Errorf("%s obbligatorio", settingUpstream))
	}
	if v := s[settingAPIVer]; v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("%s %q non valida", settingAPIVer, v))
		}
	}
	return errors.Join(errs...)
}

// NewNginxPlusPool builds the nginx-plus pool.
func NewNginxPlusPool(ctx context.Context, env Env) (Pool, error) {
	if err := validateNginxPlus(env.Settings); err != nil {
		return nil, err
	}
	c, err := httpClient(env.Settings, env.Timeout)
	if err != nil {
		return nil, err
	}
	p := &nginxPlusPool{urls: splitList(env.Settings[settingAPIURLs]), upstream: env.Settings[settingUpstream], version: env.Settings[settingAPIVer], client: c}
	if p.version == "" {
		p.version = "9"
	}
	if name := env.Settings[settingCredentials]; name != "" {
		if p.creds, err = env.Secrets.Secret(ctx, name); err != nil {
			return nil, fmt.Errorf("%s: %w", settingCredentials, err)
		}
	}
	return p, nil
}

type nginxServer struct {
	ID     int    `json:"id"`
	Server string `json:"server"`
	Weight int    `json:"weight,omitempty"`
	Drain  bool   `json:"drain,omitempty"`
	Down   bool   `json:"down,omitempty"`
}

type nginxPeer struct {
	ID     int    `json:"id"`
	Server string `json:"server"`
	State  string `json:"state"`
	Active int    `json:"active"`
}

func (p *nginxPlusPool) upstreamURL(base string) string {
	return strings.TrimRight(base, "/") + "/" + p.version + "/http/upstreams/" + url.PathEscape(p.upstream)
}

func (p *nginxPlusPool) do(ctx context.Context, method, u string, body []byte, out any) error {
	return doJSON(ctx, p.client, method, u, body, out, func(r *http.Request) {
		if user, pass, ok := strings.Cut(p.creds, ":"); ok {
			r.SetBasicAuth(user, strings.TrimSpace(pass))
		}
	})
}

func (p *nginxPlusPool) peers(ctx context.Context, base string) ([]nginxPeer, error) {
	var out struct {
		Peers []nginxPeer `json:"peers"`
	}
	if err := p.do(ctx, http.MethodGet, p.upstreamURL(base), nil, &out); err != nil {
		return nil, err
	}
	return out.Peers, nil
}

// Members implements Pool: a member is healthy and active only if it is on all instances.
func (p *nginxPlusPool) Members(ctx context.Context) ([]Member, error) {
	byName := map[string]*Member{}
	var order []string
	for _, base := range p.urls {
		peers, err := p.peers(ctx, base)
		if err != nil {
			return nil, err
		}
		for _, pe := range peers {
			m := byName[pe.Server]
			if m == nil {
				m = &Member{Name: pe.Server, State: StateActive, Healthy: true}
				byName[pe.Server] = m
				order = append(order, pe.Server)
			}
			switch pe.State {
			case "draining":
				if m.State == StateActive {
					m.State = StateDraining
				}
			case "down":
				m.State = StateDisabled
			}
			m.Healthy = m.Healthy && (pe.State == "up" || pe.State == "draining")
		}
	}
	out := make([]Member, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// setServers changes the given servers on every instance.
func (p *nginxPlusPool) setServers(ctx context.Context, members []string, body []byte) error {
	for _, base := range p.urls {
		var servers []nginxServer
		if err := p.do(ctx, http.MethodGet, p.upstreamURL(base)+"/servers", nil, &servers); err != nil {
			return err
		}
		for _, m := range members {
			id := -1
			for _, s := range servers {
				if s.Server == m {
					id = s.ID
				}
			}
			if id < 0 {
				return fmt.Errorf("%s: server %s assente dall'upstream %s", redactURL(base), m, p.upstream)
			}
			if err := p.do(ctx, http.MethodPatch, p.upstreamURL(base)+"/servers/"+strconv.Itoa(id), body, nil); err != nil {
				return fmt.Errorf("server %s: %w", m, err)
			}
		}
	}
	return nil
}

// Drain implements Pool.
func (p *nginxPlusPool) Drain(ctx context.Context, members []string) error {
	return p.setServers(ctx, members, []byte(`{"drain":true}`))
}

// Restore implements Pool.
func (p *nginxPlusPool) Restore(ctx context.Context, members []string) error {
	return p.setServers(ctx, members, []byte(`{"drain":false,"down":false}`))
}

// Connections implements Pool: active connections summed over the instances.
func (p *nginxPlusPool) Connections(ctx context.Context, members []string) (map[string]int, error) {
	out := map[string]int{}
	for _, m := range members {
		out[m] = 0
	}
	for _, base := range p.urls {
		peers, err := p.peers(ctx, base)
		if err != nil {
			return nil, err
		}
		for _, pe := range peers {
			if _, ok := out[pe.Server]; ok {
				out[pe.Server] += pe.Active
			}
		}
	}
	return out, nil
}

// Healthy implements Pool.
func (p *nginxPlusPool) Healthy(ctx context.Context, members []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range members {
		out[m] = true
	}
	for _, base := range p.urls {
		peers, err := p.peers(ctx, base)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, pe := range peers {
			if _, ok := out[pe.Server]; ok {
				seen[pe.Server] = true
				out[pe.Server] = out[pe.Server] && pe.State == "up"
			}
		}
		for _, m := range members {
			out[m] = out[m] && seen[m]
		}
	}
	return out, nil
}
