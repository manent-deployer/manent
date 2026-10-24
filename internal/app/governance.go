package app

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/policy"
	"github.com/joeg-ita/manent/internal/secrets"
)

// NewStaticGovernance creates the release rules and the freeze windows of the configuration.
func NewStaticGovernance(cfg *config.Config) (*governance.Policy, error) {
	g := cfg.Governance
	rules := make([]governance.Rule, len(g.Rules))
	for i, r := range g.Rules {
		rules[i] = governance.Rule{Name: r.Name, Applications: r.Applications, Environments: r.Environments, Requires: r.Requires, Tenants: r.Tenants, Artifacts: r.Artifacts, Selector: r.Selector}
		if a := r.Approvals; a != nil {
			rules[i].Approvals = governance.Approvals{Required: a.Required, AllowSelf: a.AllowSelf, Timeout: a.Timeout, When: a.When}
			if ap := a.Approvers; ap != nil {
				rules[i].Approvals.Approvers = &governance.Approvers{Groups: ap.Groups, Subjects: ap.Subjects}
			}
		}
		if ch := r.Change; ch != nil {
			rules[i].Change = &governance.Change{Approval: ch.Approval, Timeout: cmp.Or(ch.Timeout, 72*time.Hour)}
		}
	}
	windows := make([]governance.Window, len(g.Freezes))
	for i, f := range g.Freezes {
		windows[i] = governance.Window{
			Name: f.Name, Reason: f.Reason, Applications: f.Applications, Environments: f.Environments, Regions: f.Regions,
			From: f.From, To: f.To, Cron: f.Cron, Duration: f.Duration, Timezone: f.Timezone, Hard: f.Hard, Tenants: f.Tenants, Artifacts: f.Artifacts, Selector: f.Selector,
		}
	}
	return governance.New(rules, windows)
}

// NewGovernance creates the governance policy in force: the built-in rules of manent.yml or
// the Rego policies of governance.opa, with the freezes created at runtime. The database store
// is set with SetStore and the refresh started with Start.
func NewGovernance(cfg *config.Config, sp secrets.Provider, log *slog.Logger) (*policy.Runtime, error) {
	static, err := NewStaticGovernance(cfg)
	if err != nil {
		return nil, err
	}
	g := cfg.Governance
	o := policy.Options{
		Engine: cmp.Or(g.Engine, policy.EngineBuiltin), Static: static, Review: g.OPA.Store.Review,
		Refresh: g.Refresh, MaxStaleness: g.MaxStaleness, Logger: log,
		Compile:    policy.CompileOptions{Query: g.OPA.Query, EvalTimeout: g.OPA.EvalTimeout, RequireTests: g.OPA.Store.Tests == "required", Authz: AuthzCompileOptions(cfg)},
		AuthzCache: cfg.RBAC.OPA.Cache, AuthzCacheSize: cfg.RBAC.OPA.CacheSize,
	}
	if o.Engine == policy.EngineOPA {
		o.SourceName = cmp.Or(g.OPA.Source, policy.SourceStore)
		if o.Source, err = newPolicySource(cfg, sp, log); err != nil {
			return nil, err
		}
	}
	return policy.New(o)
}

// newPolicySource creates the source of the Rego policies other than the store.
func newPolicySource(cfg *config.Config, sp secrets.Provider, log *slog.Logger) (policy.Source, error) {
	o := cfg.Governance.OPA
	switch o.Source {
	case config.PolicySourceFile:
		src := &policy.FileSource{Dir: o.File.Dir}
		for _, p := range src.Permissions() {
			log.Warn("policy file writable by everyone: anyone on the host can change the governance", "path", p)
		}
		return src, nil
	case config.PolicySourceBundle:
		b := o.Bundle
		client := &http.Client{Timeout: 30 * time.Second}
		if b.CAFile != "" {
			pem, err := os.ReadFile(b.CAFile)
			if err != nil {
				return nil, fmt.Errorf("governance.opa.bundle.ca_file: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("governance.opa.bundle.ca_file %s: no PEM certificate", b.CAFile)
			}
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			client.Transport = tr
		}
		// The bundle server must not redirect the download (and the token) elsewhere.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		src := &policy.BundleSource{URL: b.URL, Client: client}
		if b.CredentialsRef != "" {
			ref := b.CredentialsRef
			src.Token = func(ctx context.Context) (string, error) { return sp.Resolve(ctx, ref) }
		}
		if b.Signing.PublicKeyRef != "" {
			key, err := sp.Resolve(context.Background(), b.Signing.PublicKeyRef)
			if err != nil {
				return nil, fmt.Errorf("governance.opa.bundle.signing.public_key_ref: %w", err)
			}
			src.Verification = policy.NewVerification(key, b.Signing.Algorithm, b.Signing.KeyID, b.Signing.Scope)
		}
		return src, nil
	case config.PolicySourceConsul:
		addr, token, err := consulAccess(cfg)
		if err != nil {
			return nil, err
		}
		return &policy.ConsulSource{Address: addr, Token: token, Datacenter: cfg.EnvConfig.Consul.Datacenter,
			Prefix: cmp.Or(o.Consul.Prefix, "manent/policies"), Client: &http.Client{Timeout: 10 * time.Second}}, nil
	}
	return nil, nil
}

// consulAccess returns address and ACL token of Consul (envconfig.consul, CONSUL_HTTP_ADDR,
// CONSUL_HTTP_TOKEN or token_file).
func consulAccess(cfg *config.Config) (addr, token string, err error) {
	c := cfg.EnvConfig.Consul
	addr = c.Address
	if addr == "" {
		addr = cmp.Or(os.Getenv("CONSUL_HTTP_ADDR"), "http://127.0.0.1:8500")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	token = os.Getenv("CONSUL_HTTP_TOKEN")
	if c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", "", fmt.Errorf("reading the Consul token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	return addr, token, nil
}
