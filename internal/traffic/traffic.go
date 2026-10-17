// Package traffic integrates manent with load balancers and traffic routers: it drains the
// targets from a pool before updating them and puts them back in service afterwards (Pool),
// and it shifts traffic between two groups of targets, the slots, in percentage rollouts (Router).
//
// Providers that speak only HTTP or SSH live here; those using an SDK (e.g. AWS) live among
// the plugins and are registered in Providers by internal/app.
package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// States of a pool member.
const (
	StateActive   = "active"
	StateDraining = "draining"
	StateDisabled = "disabled"
)

// Member is the state of a pool member.
type Member struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Healthy bool   `json:"healthy"`
}

// InService means an active and healthy member that receives traffic.
func (m Member) InService() bool { return m.State == StateActive && m.Healthy }

// Pool is a load balancer pool. All methods are idempotent: retries and recoveries can repeat them.
type Pool interface {
	// Members returns the state of all pool members, for the capacity check; nil if the provider
	// does not know it.
	Members(ctx context.Context) ([]Member, error)
	// Drain stops sending new requests to the members; requests in progress continue.
	Drain(ctx context.Context, members []string) error
	// Connections returns the still active connections of every member.
	Connections(ctx context.Context, members []string) (map[string]int, error)
	// Restore puts the members back in service.
	Restore(ctx context.Context, members []string) error
	// Healthy reports, for every member, whether it is in service and healthy for the load balancer.
	Healthy(ctx context.Context, members []string) (map[string]bool, error)
}

// DrainDelayer is implemented by pools that cannot count connections: after Drain the wait is
// always at least DrainDelay (e.g. the time the load balancer takes to notice a red health check).
type DrainDelayer interface {
	DrainDelay() time.Duration
}

// Router shifts traffic between slots: every slot is the group of targets with the slot label.
type Router interface {
	// Weights returns the traffic share of every slot (0-100).
	Weights(ctx context.Context) (map[string]int, error)
	// SetWeights sets the shares; idempotent.
	SetWeights(ctx context.Context, weights map[string]int) error
}

// TesterRouter is implemented by routers that can route only the testers' requests to a slot.
type TesterRouter interface {
	RouteTesters(ctx context.Context, slot string) (map[string]string, error)
}

// Env is what is needed to build a provider.
type Env struct {
	// Settings are the provider settings (traffic.pool.settings or traffic.router.settings).
	Settings map[string]string
	// Member computes the member name of a target.
	Member func(descriptor.Target) string
	// Targets are all the deploy targets (for providers that work on the targets).
	Targets []descriptor.Target
	// Secrets resolves secrets by name, at use time.
	Secrets plugin.SecretResolver
	// DeploymentID identifies the deploy in calls (e.g. STS session name).
	DeploymentID string
	// Dialer opens SSH sessions (target-based providers).
	Dialer  Dialer
	Logger  *slog.Logger
	Timeout time.Duration
}

// PoolFactory builds a pool; ValidatePool checks its settings without side effects.
type (
	PoolFactory   func(ctx context.Context, env Env) (Pool, error)
	RouterFactory func(ctx context.Context, env Env) (Router, error)
	Validator     func(settings map[string]string) error
)

// Provider is a registered provider: constructor and settings check.
type Provider[F any] struct {
	New      F
	Validate Validator
}

// Providers are the available providers, by name.
type Providers struct {
	Pools   map[string]Provider[PoolFactory]
	Routers map[string]Provider[RouterFactory]
}

// Default returns this package's providers; SDK-based ones are added by internal/app.
func Default() *Providers {
	return &Providers{
		Pools: map[string]Provider[PoolFactory]{
			"haproxy":     {New: NewHAProxyPool, Validate: validateHAProxy},
			"health-flag": {New: NewHealthFlagPool, Validate: validateHealthFlag},
			"f5":          {New: NewF5Pool, Validate: validateF5},
			"nginx-plus":  {New: NewNginxPlusPool, Validate: validateNginxPlus},
			"nginx":       {New: NewNginxPool, Validate: validateNginx},
		},
		Routers: map[string]Provider[RouterFactory]{
			"haproxy":    {New: NewHAProxyRouter, Validate: validateHAProxy},
			"f5":         {New: NewF5Router, Validate: validateF5},
			"nginx-plus": {New: NewNginxPlusRouter, Validate: validateNginxPlus},
		},
	}
}

// Check checks an environment's traffic configuration: known providers and valid settings.
// It does not contact the systems.
func (p *Providers) Check(c *descriptor.TrafficConfig) error {
	if c == nil {
		return nil
	}
	var errs []error
	if c.Pool != nil {
		pr, ok := p.Pools[c.Pool.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown traffic.pool.provider %q (%s)", c.Pool.Provider, strings.Join(slices.Sorted(maps.Keys(p.Pools)), ", ")))
		} else if pr.Validate != nil {
			if err := pr.Validate(c.Pool.Settings); err != nil {
				errs = append(errs, fmt.Errorf("traffic.pool (%s): %w", c.Pool.Provider, err))
			}
		}
	}
	if c.Router != nil {
		pr, ok := p.Routers[c.Router.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown traffic.router.provider %q (%s)", c.Router.Provider, strings.Join(slices.Sorted(maps.Keys(p.Routers)), ", ")))
		} else if pr.Validate != nil {
			if err := pr.Validate(c.Router.Settings); err != nil {
				errs = append(errs, fmt.Errorf("traffic.router (%s): %w", c.Router.Provider, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Pool builds the environment pool.
func (p *Providers) Pool(ctx context.Context, c *descriptor.PoolConfig, env Env) (Pool, error) {
	pr, ok := p.Pools[c.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown traffic.pool.provider %q", c.Provider)
	}
	env.Settings = c.Settings
	env.Member = c.MemberName
	return pr.New(ctx, env)
}

// Router builds the environment router.
func (p *Providers) Router(ctx context.Context, c *descriptor.RouterConfig, env Env) (Router, error) {
	pr, ok := p.Routers[c.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown traffic.router.provider %q", c.Provider)
	}
	env.Settings = c.Settings
	// The server name of a target in load balancer routers (member setting, default "{host}").
	tmpl := c.Settings["member"]
	env.Member = func(t descriptor.Target) string { return descriptor.ExpandTarget(tmpl, t) }
	return pr.New(ctx, env)
}

// Capacity checks that, removing the given members, at least minHealthy healthy members stay
// in service. Members already out of service do not count.
func Capacity(all []Member, drain []string, minHealthy int) error {
	in := 0
	for _, m := range all {
		if m.InService() && !slices.Contains(drain, m.Name) {
			in++
		}
	}
	if in < minHealthy {
		return fmt.Errorf("insufficient capacity: draining %s would leave %d healthy members in service out of %d, minimum %d",
			strings.Join(drain, ", "), in, len(all), minHealthy)
	}
	return nil
}

// Missing returns the given members the pool does not contain.
func Missing(all []Member, names []string) []string {
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(all, func(m Member) bool { return m.Name == n }) {
			out = append(out, n)
		}
	}
	return out
}

// httpClient builds the providers' HTTP client: TLS 1.2+, dedicated CA (ca_file), no redirects.
func httpClient(settings map[string]string, timeout time.Duration) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if f := settings["ca_file"]; f != "" {
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no certificate", f)
		}
		tc.RootCAs = pool
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// checkURL checks a management API URL: https, unless allow_http.
func checkURL(settings map[string]string, key, u string) error {
	switch {
	case strings.HasPrefix(u, "https://"):
		return nil
	case strings.HasPrefix(u, "http://") && settings["allow_http"] == "true":
		return nil
	}
	return fmt.Errorf("%s %q: an https URL is required (http only with allow_http: \"true\")", key, u)
}

// splitList reads a comma-separated list.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
