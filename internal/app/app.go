// Package app wires all manent components from the configuration.
// It is the only place where concrete implementations are bound to interfaces.
package app

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gin-gonic/gin"

	"github.com/joeg-ita/manent/internal/api"
	"github.com/joeg-ita/manent/internal/artifact"
	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/auth"
	"github.com/joeg-ita/manent/internal/authz"
	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/cluster"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/deploylog"
	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/envconfig"
	"github.com/joeg-ita/manent/internal/extplugin"
	"github.com/joeg-ita/manent/internal/gitsrc"
	"github.com/joeg-ita/manent/internal/logging"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/policy"
	"github.com/joeg-ita/manent/internal/rollback"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/steps"
	"github.com/joeg-ita/manent/internal/telemetry"
	"github.com/joeg-ita/manent/internal/tenant"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/internal/version"
	"github.com/joeg-ita/manent/internal/webui"
	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/appservice"
	"github.com/joeg-ita/manent/plugins/awselb"
	"github.com/joeg-ita/manent/plugins/awsmetrics"
	"github.com/joeg-ita/manent/plugins/azuremonitor"
	"github.com/joeg-ita/manent/plugins/cloudrun"
	"github.com/joeg-ita/manent/plugins/container"
	"github.com/joeg-ita/manent/plugins/containerapps"
	"github.com/joeg-ita/manent/plugins/ecs"
	"github.com/joeg-ita/manent/plugins/jboss"
	"github.com/joeg-ita/manent/plugins/kubernetes"
	"github.com/joeg-ita/manent/plugins/lambda"
	"github.com/joeg-ita/manent/plugins/remotecopy"
	"github.com/joeg-ita/manent/plugins/windows"
)

// App holds the components shared by CLI and API.
type App struct {
	Config *config.Config
	Logger *slog.Logger
	// Service applies the permissions of the identity in the context (internal/authz).
	Service engine.Service
	plugins *plugin.Registry
	closers []io.Closer
	worker  *engine.Worker // set in worker mode
	client  dbos.Client    // set in client mode
	siem    logging.SIEMSender
	audit   audit.Recorder
	// metrics is nil when observability.metrics is false.
	metrics  *telemetry.Metrics
	registry cluster.Registry
	// admin is created on first use: CLI commands that do not use it need no registry.
	// slack receives the approval buttons of Slack messages (nil if not configured).
	slack     http.Handler
	adminOnce sync.Once
	admin     *engine.Admin
	draining  atomic.Bool
	// engine is the service without permissions (platform operations of manent-server admin).
	engine *engine.Engine
	// governance is the policy in force; stopGovernance ends its refresh.
	governance     *policy.Runtime
	stopGovernance context.CancelFunc
}

// NewLogger creates the application logger, with forwarding to the SIEM if configured.
func NewLogger(cfg *config.Config) (*slog.Logger, logging.SIEMSender, error) {
	var sender logging.SIEMSender = logging.NoopSender{}
	if targets := cfg.SIEM.Targets(); len(targets) > 0 {
		senders := make(logging.MultiSender, 0, len(targets))
		for _, d := range targets {
			s, err := logging.NewSender(d.Network, d.Address)
			if err != nil {
				return nil, nil, err
			}
			senders = append(senders, s)
		}
		sender = senders
		if len(senders) == 1 {
			sender = senders[0]
		}
	}
	log := logging.New(logging.Options{
		AppName: cfg.App.Name,
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		SIEM:    sender,
	})
	// Logs emitted during a step also go to the deploy log (DBOS stream).
	return slog.New(deploylog.NewHandler(log.Handler())), sender, nil
}

// SetupTracing configures OpenTelemetry tracing for long-running processes (serve, worker).
// instance identifies the process in traces (e.g. executor_id).
func SetupTracing(ctx context.Context, cfg *config.Config, instance string) (func(context.Context) error, error) {
	t := cfg.Observability.Tracing
	return telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled: t.Enabled, Endpoint: t.Endpoint, Insecure: t.Insecure, SampleRatio: t.SampleRatio,
		Service: cfg.App.Name, Version: version.Version, Instance: instance,
	})
}

// recorder returns the metrics Recorder, or one that discards them.
func recorder(m *telemetry.Metrics) telemetry.Recorder {
	if m == nil {
		return telemetry.Noop{}
	}
	return m
}

// NewPlugins registers the available plugins and activates the configured ones.
// Plugins compiled into manent are added here with Register; external plugins
// ("external" key in the configuration) are registered automatically with their name.
func NewPlugins(cfg *config.Config, log *slog.Logger) (*plugin.Registry, error) {
	reg := plugin.NewRegistry()
	for name, f := range map[string]plugin.Factory{
		remotecopy.Name:    remotecopy.Factory,
		container.Name:     container.Factory,
		kubernetes.Name:    kubernetes.Factory,
		ecs.Name:           ecs.Factory,
		lambda.Name:        lambda.Factory,
		cloudrun.Name:      cloudrun.Factory,
		jboss.Name:         jboss.Factory,
		windows.Name:       windows.Factory,
		appservice.Name:    appservice.Factory,
		containerapps.Name: containerapps.Factory,
	} {
		if err := reg.Register(name, f); err != nil {
			return nil, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Plugins)) {
		pc := cfg.Plugins[name]
		if extplugin.IsExternal(pc) {
			if err := reg.Register(name, extplugin.NewFactory(name)); err != nil {
				return nil, fmt.Errorf("external plugin: %w", err)
			}
		}
		if err := reg.Activate(name, pc, log); err != nil {
			_ = reg.Close()
			return nil, err
		}
	}
	return reg, nil
}

// NewLocators returns the strategies that find the release to go back to on rollback, by
// name (rollback.locator). A locator that queries a company system of releases in service is
// added here, implementing plugin.ReleaseLocator.
func NewLocators(cfg *config.Config, log *slog.Logger) map[string]plugin.ReleaseLocator {
	return map[string]plugin.ReleaseLocator{
		plugin.PluginLocatorName: plugin.PluginLocator{},
	}
}

// NewRollbackPolicy creates the rollback policy from the configuration and the registered locators.
func NewRollbackPolicy(cfg *config.Config, locators map[string]plugin.ReleaseLocator) rollback.Policy {
	return rollback.NewPolicy(cfg, slices.Sorted(maps.Keys(locators)))
}

// NewSteps creates the registry of the standard steps. The artifact store is shared by the
// artifacts step, the deploy step (which downloads again a missing file) and the final cleanup.
// It also returns the load balancer and router operations used by the engine (batch draining
// and percentage rollouts), which share the deploy plugin and the secrets.
func NewSteps(plugins *plugin.Registry, store *artifact.Store, sp secrets.Provider, locators map[string]plugin.ReleaseLocator, log *slog.Logger) (*pipeline.Registry, *steps.Traffic, error) {
	stepLog := log.With("component", "step")
	deploy := steps.Deploy{
		Policies:  map[string]steps.Policy{"": steps.DefaultPolicy{Plugins: plugins}, "default": steps.DefaultPolicy{Plugins: plugins}},
		Secrets:   sp,
		Artifacts: store,
		Locators:  locators,
		Logger:    stepLog,
	}
	providers := NewTrafficProviders()
	pool := steps.Pool{Providers: providers, Secrets: sp, Logger: log.With("component", "traffic")}
	reg, err := pipeline.NewRegistry(
		steps.Secrets{Provider: sp, Logger: stepLog},
		steps.Artifacts{Store: store, Logger: stepLog},
		steps.QA{Checkers: []steps.Checker{steps.NonEmptyChecker{}, steps.ChecksumChecker{}}, Logger: stepLog},
		deploy,
		steps.PostTest{Client: &http.Client{Timeout: 10 * time.Second}, Logger: stepLog},
		steps.Activation{Activator: steps.LogActivator{Logger: stepLog}},
		steps.Drain{Pool: pool},
		steps.Restore{Pool: pool},
	)
	if err != nil {
		return nil, nil, err
	}
	return reg, &steps.Traffic{Deploy: deploy, Pool: pool, Providers: providers, Logger: log.With("component", "traffic")}, nil
}

// NewTrafficProviders returns the load balancer and router providers: the HTTP and SSH ones
// of internal/traffic, those based on cloud SDKs and the Windows ones (ARR, NLB) of the
// windows plugin.
func NewTrafficProviders() *traffic.Providers {
	p := traffic.Default()
	p.Pools["aws-elb"] = traffic.Provider[traffic.PoolFactory]{New: awselb.NewPool(awselb.SDKConnector{}), Validate: awselb.ValidatePool}
	p.Routers["aws-alb"] = traffic.Provider[traffic.RouterFactory]{New: awselb.NewRouter(awselb.SDKConnector{}), Validate: awselb.ValidateRouter}
	p.Pools[windows.PoolARR] = traffic.Provider[traffic.PoolFactory]{New: windows.NewPoolFactory(windows.PoolARR, windows.DefaultPoolConnector), Validate: windows.ValidateARR}
	p.Pools[windows.PoolNLB] = traffic.Provider[traffic.PoolFactory]{New: windows.NewPoolFactory(windows.PoolNLB, windows.DefaultPoolConnector), Validate: windows.ValidateNLB}
	return p
}

// NewSecretProvider creates the secret provider with the env:, file: and, if enabled, vault: schemes.
func NewSecretProvider(cfg *config.Config) (*secrets.SchemeProvider, error) {
	sp := secrets.NewDefaultProvider()
	if v := cfg.Secrets.Vault; v.Enabled {
		vault, err := secrets.NewVault(secrets.VaultConfig{
			Address: v.Address, Namespace: v.Namespace, Auth: v.Auth, TokenFile: v.TokenFile,
			RoleID: v.AppRole.RoleID, SecretIDFile: v.AppRole.SecretIDFile,
			Role: v.Kubernetes.Role, JWTFile: v.Kubernetes.JWTFile,
			Mount: cmp.Or(v.AppRole.Mount, v.Kubernetes.Mount), CAFile: v.CAFile, Timeout: v.Timeout,
		})
		if err != nil {
			return nil, err
		}
		sp.Register("vault", vault)
	}
	return sp, nil
}

// NewArtifactSources creates the configured artifact sources, with their credentials.
func NewArtifactSources(cfg *config.Config, sp secrets.Provider) (*artifact.Sources, error) {
	list := make([]artifact.Source, len(cfg.Artifacts.Sources))
	for i, s := range cfg.Artifacts.Sources {
		list[i] = artifact.Source{Prefix: s.Prefix, CAFile: s.CAFile}
		if a := s.Auth; a != nil {
			list[i].Auth = &artifact.SourceAuth{Type: a.Type, Username: a.Username, Header: a.Header, SecretRef: a.SecretRef}
		}
	}
	allow := cfg.Artifacts.AllowUnlisted == nil || *cfg.Artifacts.AllowUnlisted
	return artifact.NewSources(list, allow, sp, &http.Client{Timeout: cfg.Artifacts.Timeout})
}

// NewAuthenticator creates the API authentication: static tokens and, if configured, OIDC
// (with issuer discovery). It returns nil with auth.mode none.
func NewAuthenticator(ctx context.Context, cfg *config.Config, log *slog.Logger) (auth.Authenticator, error) {
	if cfg.Auth.Mode == config.AuthModeNone {
		return nil, nil
	}
	var chain auth.Chain
	if len(cfg.Auth.Tokens) > 0 {
		ts := make(auth.Tokens, len(cfg.Auth.Tokens))
		for i, t := range cfg.Auth.Tokens {
			ts[i] = auth.StaticToken{Name: t.Name, SHA256: t.SHA256, Groups: t.Groups, ExpiresAt: t.ExpiresAt}
		}
		chain = append(chain, ts)
	}
	if cfg.Auth.TokensFile != "" {
		tf, err := auth.NewTokenFile(cfg.Auth.TokensFile, log.With("component", "auth"))
		if err != nil {
			return nil, err
		}
		chain = append(chain, tf)
	}
	if o := cfg.Auth.OIDC; o != nil {
		v, err := auth.NewOIDC(ctx, auth.OIDCConfig{
			Issuer: o.Issuer, Audience: o.Audience, UsernameClaim: o.UsernameClaim, GroupsClaim: o.GroupsClaim, CAFile: o.CAFile,
			PolicyClaims: policyClaims(o),
		})
		if err != nil {
			return nil, err
		}
		chain = append(chain, v)
	}
	return chain, nil
}

// policyClaims are the claims exposed to the permission policy: the default ones (email,
// name) unless auth.oidc.policy_claims sets them (also to an empty list).
func policyClaims(o *config.OIDC) []string {
	if o.PolicyClaims == nil {
		return config.DefaultPolicyClaims
	}
	return o.PolicyClaims
}

// NewAuthzPolicy creates the RBAC policy; with auth.mode none it allows everything.
func NewAuthzPolicy(cfg *config.Config) (authz.Policy, error) {
	if cfg.Auth.Mode == config.AuthModeNone {
		return authz.Policy{Disabled: true}, nil
	}
	roles, bindings := RBACConfig(cfg)
	return authz.NewPolicy(roles, bindings)
}

// RBACConfig converts rbac.roles and rbac.bindings of the configuration.
func RBACConfig(cfg *config.Config) (map[string][]authz.Action, []authz.Binding) {
	roles := map[string][]authz.Action{}
	for name, acts := range cfg.RBAC.Roles {
		for _, a := range acts {
			roles[name] = append(roles[name], authz.Action(a))
		}
	}
	bindings := make([]authz.Binding, len(cfg.RBAC.Bindings))
	for i, b := range cfg.RBAC.Bindings {
		bindings[i] = authz.Binding{Subjects: b.Subjects, Groups: b.Groups, Role: b.Role, Applications: b.Applications, Environments: b.Environments, Tenants: b.Tenants, Artifacts: b.Artifacts, Selector: b.Selector}
	}
	return roles, bindings
}

// AuthzCompileOptions returns the permission options of the revisions: nil unless rbac.engine
// is opa.
func AuthzCompileOptions(cfg *config.Config) *policy.AuthzOptions {
	if cfg.RBAC.Engine != config.RBACOPA {
		return nil
	}
	roles, bindings := RBACConfig(cfg)
	return &policy.AuthzOptions{Roles: roles, Bindings: bindings}
}

// NewAuditRecorder creates the audit recorder: application log (and SIEM) and, if configured,
// a hash-chained file (HMAC if a key is set).
func NewAuditRecorder(cfg *config.Config, sp secrets.Provider, log *slog.Logger) (audit.Multi, io.Closer, error) {
	rec := audit.Multi{audit.LogRecorder{Logger: log.With("component", "audit")}}
	if cfg.Audit.File == "" {
		return rec, io.NopCloser(nil), nil
	}
	key, err := AuditKey(cfg, sp)
	if err != nil {
		return nil, nil, err
	}
	f, err := audit.NewFileRecorder(cfg.Audit.File, audit.FileOptions{MaxSize: int64(cfg.Audit.MaxSizeMB) << 20, Key: key})
	if err != nil {
		return nil, nil, err
	}
	return append(rec, f), f, nil
}

// AuditKey reads the HMAC key of the audit chain, if configured.
func AuditKey(cfg *config.Config, sp secrets.Provider) ([]byte, error) {
	if cfg.Audit.HMACKeyRef == "" {
		return nil, nil
	}
	v, err := sp.Resolve(context.Background(), cfg.Audit.HMACKeyRef)
	if err != nil {
		return nil, fmt.Errorf("audit.hmac_key_ref: %w", err)
	}
	if len(v) < 16 {
		return nil, errors.New("audit.hmac_key_ref: key too short (at least 16 bytes)")
	}
	return []byte(v), nil
}

// NewHealthChecker creates the health gate of rollouts: HTTP probes, metrics from Prometheus,
// Datadog, New Relic, Elastic and CloudWatch (rollout.<system> in manent.yml) and
// Alertmanager. Another metrics system is added here, implementing rollout.Checker.
func NewHealthChecker(cfg *config.Config, sp secrets.Provider) (rollout.Checker, error) {
	client := &http.Client{Timeout: cfg.Rollout.CheckTimeout}
	r := cfg.Rollout
	sources := map[string]rollout.MetricSource{}
	secret := rollout.SecretFunc(sp.Resolve)
	if r.PrometheusURL != "" {
		sources[descriptor.ProviderPrometheus] = &rollout.PrometheusSource{URL: r.PrometheusURL, Client: client}
	}
	if d := r.Datadog; d != nil {
		sources[descriptor.ProviderDatadog] = &rollout.DatadogSource{URL: d.URL, APIKeyRef: d.APIKeyRef, AppKeyRef: d.AppKeyRef, Secret: secret, Client: client}
	}
	if n := r.NewRelic; n != nil {
		sources[descriptor.ProviderNewRelic] = &rollout.NewRelicSource{URL: n.URL, AccountID: n.AccountID, APIKeyRef: n.APIKeyRef, Secret: secret, Client: client}
	}
	if e := r.Elastic; e != nil {
		ec := client
		if e.CAFile != "" {
			pem, err := os.ReadFile(e.CAFile)
			if err != nil {
				return nil, fmt.Errorf("rollout.elastic.ca_file: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("rollout.elastic.ca_file %s: no PEM certificate", e.CAFile)
			}
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			ec = &http.Client{Timeout: cfg.Rollout.CheckTimeout, Transport: tr}
		}
		sources[descriptor.ProviderElastic] = &rollout.ElasticSource{URL: e.URL, APIKeyRef: e.APIKeyRef, Username: e.Username, PasswordRef: e.PasswordRef, Secret: secret, Client: ec}
	}
	if w := r.CloudWatch; w != nil {
		sources[descriptor.ProviderCloudWatch] = &awsmetrics.Source{Region: w.Region, Endpoint: w.Endpoint, AccessKeyIDRef: w.AccessKeyIDRef,
			SecretAccessKeyRef: w.SecretAccessKeyRef, RoleARN: w.RoleARN, ExternalID: w.ExternalID, Secret: secret}
	}
	if a := r.AzureMonitor; a != nil {
		sources[descriptor.ProviderAzureMonitor] = &azuremonitor.Source{WorkspaceID: a.WorkspaceID, Endpoint: a.Endpoint, Authority: a.Authority,
			TenantID: a.TenantID, ClientID: a.ClientID, ClientSecretRef: a.ClientSecretRef, Secret: secret, Client: client}
	}
	return &rollout.HTTPChecker{Client: client, Sources: sources, AlertmanagerURL: r.AlertmanagerURL}, nil
}

// NewDescriptorPolicy returns the descriptor policy of the configuration.
func NewDescriptorPolicy(cfg *config.Config) *envconfig.DescriptorPolicy {
	dp := cfg.DescriptorPolicy
	return &envconfig.DescriptorPolicy{Params: dp.Params, SecretRefs: dp.SecretRefs, Targets: dp.Targets == nil || *dp.Targets, StepParams: dp.StepParams, ConfigRef: cmp.Or(dp.ConfigRef, envconfig.ConfigRefNone)}
}

// NewResolver creates the environment configuration resolver for the configured provider;
// secrets resolves the credentials of the git provider.
func NewResolver(cfg *config.Config, secrets gitsrc.Secrets) (envconfig.Resolver, error) {
	ec := cfg.EnvConfig
	r := envconfig.Resolver{Required: ec.Required, Policy: NewDescriptorPolicy(cfg)}
	switch ec.Provider {
	case config.EnvProviderConsul:
		addr, token, err := consulAccess(cfg)
		if err != nil {
			return r, err
		}
		r.Provider = &envconfig.ConsulProvider{
			Address:     addr,
			Token:       token,
			Datacenter:  ec.Consul.Datacenter,
			KeyTemplate: ec.Consul.Key,
			Client:      &http.Client{Timeout: 10 * time.Second},
		}
	case config.EnvProviderFile:
		r.Provider = &envconfig.FileProvider{PathTemplate: ec.File.Path}
	case config.EnvProviderGit:
		g := ec.Git
		repo, err := gitsrc.New(gitsrc.Options{
			Repository: g.Repository, DefaultRef: g.Ref, AllowedRefs: g.AllowedRefs,
			Username: g.Username, PasswordRef: g.PasswordRef, SSHKeyRef: g.SSHKeyRef, KnownHosts: g.KnownHosts, CAFile: g.CAFile,
			RequireSigned: g.RequireSigned, OpenPGPKeyring: g.Signers.OpenPGPKeyring, SSHAllowedSigners: g.Signers.SSHAllowedSigners,
			MaxAge: g.MaxAge, CacheDir: cmp.Or(g.CacheDir, filepath.Join(cfg.WorkDir, ".git-cache")), RefTTL: g.RefTTL, Secrets: secrets,
		})
		if err != nil {
			return r, err
		}
		r.Provider = &envconfig.GitProvider{Repo: repo, PathTemplate: g.Path}
	}
	return r, nil
}

// NewTenantCache returns the tenant registry (tenants.file) with a cache for owners,
// selectors and permissions; nil without a registry.
func NewTenantCache(cfg *config.Config) tenant.Directory {
	if cfg.Tenants.File == "" {
		return nil
	}
	return &tenant.Cache{Next: tenant.File{Path: cfg.Tenants.File}, TTL: cfg.Tenants.Cache}
}

// tenantQueues returns the deploy queues of the tenants and, with dbos.tenants, the queues the
// worker listens to (the default one for the tenants without their own).
func tenantQueues(cfg *config.Config, reg tenant.Directory, log *slog.Logger) (all []engine.QueueOptions, listen []string, err error) {
	if reg == nil {
		if len(cfg.DBOS.Tenants) > 0 {
			return nil, nil, errors.New("dbos.tenants requires a tenant registry (tenants.file)")
		}
		return nil, nil, nil
	}
	ts, err := reg.Tenants(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("reading the tenant registry: %w", err)
	}
	for _, t := range ts {
		if t.Queue != "" {
			all = append(all, engine.QueueOptions{Name: t.Queue, GlobalConcurrency: t.Concurrency})
		}
	}
	for _, name := range cfg.DBOS.Tenants {
		i := slices.IndexFunc(ts, func(t tenant.Tenant) bool { return t.Name == name })
		if i < 0 {
			return nil, nil, fmt.Errorf("dbos.tenants: tenant %s is not in the registry", name)
		}
		q := cmp.Or(ts[i].Queue, cfg.DBOS.Queue)
		if !slices.Contains(listen, q) {
			listen = append(listen, q)
		}
	}
	if len(listen) > 0 {
		log.Info("worker dedicato ai tenant", "tenants", cfg.DBOS.Tenants, "queues", listen)
	}
	return all, listen, nil
}

// NewCallbackPolicy returns the limits on callback URLs set in the configuration.
func NewCallbackPolicy(cfg *config.Config) callback.Policy {
	return callback.Policy{AllowHTTP: cfg.Callbacks.AllowHTTP, AllowedHosts: cfg.Callbacks.AllowedHosts}
}

// components holds the components shared by worker and client.
type components struct {
	log      *slog.Logger
	plugins  *plugin.Registry
	siem     logging.SIEMSender
	steps    *pipeline.Registry
	store    *artifact.Store
	planner  pipeline.Planner
	resolver envconfig.Resolver
	policy   callback.Policy
	notifier callback.Notifier
	rollback rollback.Policy
	sources  *artifact.Sources
	authz    authz.Policy
	// authzOPA: rbac.engine opa, the governance runtime decides the permissions.
	authzOPA   bool
	audit      audit.Recorder
	governance *policy.Runtime
	// tenants is the tenant registry with the cache (nil without registry).
	tenants tenant.Directory
	custom  *customstep.Builder
	secrets secrets.Provider
	chat    *chat.Notifier
	change  change.Provider
	health  rollout.Checker
	traffic *steps.Traffic
	metrics *telemetry.Metrics
	closers []io.Closer
	// tenantRoles and crossTenant configure the permissions of the tenants (rbac).
	tenantRoles config.TenantRoles
	crossTenant bool
}

// service builds the service exposed to CLI and API: the Engine wrapped by permissions.
// executors provides the worker list (Admin, opened on first use in client mode).
func (c *components) service(cfg *config.Config, client dbos.Client, executors engine.ExecutorLister) engine.Service {
	return c.guard(c.engine(cfg, client, executors))
}

// engine builds the Engine without permissions: the worker uses it for release rollbacks,
// already authorized at submission.
func (c *components) engine(cfg *config.Config, client dbos.Client, executors engine.ExecutorLister) *engine.Engine {
	opts := engine.ServiceOptions{
		Client: client, QueueName: cfg.DBOS.Queue, Planner: c.planner,
		CallbackPolicy: c.policy, Rollback: c.rollback, Sources: c.sources,
		Resolver: c.resolver, Governance: c.governance, Policies: c.governance, Tenants: c.tenants, Health: c.health, Traffic: c.traffic, Logger: c.log,
		Executors: executors, StaleAfter: cfg.Recovery.StaleAfter,
		QueueLimits: engine.QueueOptions{
			WorkerConcurrency:    cfg.DBOS.Concurrency,
			GlobalConcurrency:    cfg.DBOS.GlobalConcurrency,
			PartitionConcurrency: cfg.DBOS.EnvironmentConcurrency,
		},
	}
	if r, ok := executors.(engine.TargetStateReader); ok {
		opts.TargetStates = r
	}
	if c.chat != nil {
		opts.Notifications = c.chat
	}
	return engine.NewService(opts)
}

// startGovernance loads the governance policy and keeps it up to date; the returned function
// stops the refresh.
func (c *components) startGovernance() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	c.governance.Start(ctx)
	return cancel
}

// chatNotifier returns the notifier as an interface, nil without destinations.
func (c *components) chatNotifier() engine.ChatNotifier {
	if c.chat == nil {
		return nil
	}
	return c.chat
}

// guard wraps the Engine with permissions.
func (c *components) guard(eng *engine.Engine) engine.Service {
	g := &authz.Guard{Next: eng, Policy: c.authz, Audit: c.audit, Registry: c.tenants, Logger: c.log.With("component", "authz"),
		TenantRoles: authz.TenantRoles{Members: c.tenantRoles.Members, Admins: c.tenantRoles.Admins}, CrossTenantInventory: c.crossTenant}
	if c.authzOPA {
		g.Authorizer = c.governance
	}
	return g
}

func newComponents(cfg *config.Config) (*components, error) {
	log, sender, err := NewLogger(cfg)
	if err != nil {
		return nil, err
	}
	plugins, err := NewPlugins(cfg, log)
	if err != nil {
		return nil, err
	}
	sp, err := NewSecretProvider(cfg)
	if err != nil {
		return nil, err
	}
	resolver, err := NewResolver(cfg, sp)
	if err != nil {
		return nil, err
	}
	sources, err := NewArtifactSources(cfg, sp)
	if err != nil {
		return nil, err
	}
	store := &artifact.Store{Fetcher: artifact.DefaultFetcher{Sources: sources}, WorkDir: cfg.WorkDir}
	locators := NewLocators(cfg, log)
	reg, trafficOps, err := NewSteps(plugins, store, sp, locators, log)
	if err != nil {
		return nil, err
	}
	authzPolicy, err := NewAuthzPolicy(cfg)
	if err != nil {
		return nil, err
	}
	auditRec, auditCloser, err := NewAuditRecorder(cfg, sp, log)
	if err != nil {
		return nil, err
	}
	gov, err := NewGovernance(cfg, sp, log)
	if err != nil {
		return nil, err
	}
	specKeys, err := NewSpecKeys(cfg, sp)
	if err != nil {
		return nil, err
	}
	policy := NewCallbackPolicy(cfg)
	notifier := callback.HTTPNotifier{
		Client:  callback.NewHTTPClient(cfg.Callbacks.Timeout),
		Secrets: sp,
		Policy:  policy,
		Logger:  log.With("component", "callback"),
	}
	var metrics *telemetry.Metrics
	if cfg.Observability.Metrics {
		metrics = telemetry.NewMetrics(telemetry.BuildInfo{Version: version.Version, Commit: version.Get().Commit, WorkflowVersion: cfg.App.Version})
	}
	health, err := NewHealthChecker(cfg, sp)
	if err != nil {
		return nil, err
	}
	if metrics != nil {
		if err := metrics.Register(gov.Collector()); err != nil {
			return nil, err
		}
	}
	return &components{
		metrics:     metrics,
		health:      health,
		traffic:     trafficOps,
		plugins:     plugins,
		policy:      policy,
		notifier:    notifier,
		log:         log,
		siem:        sender,
		steps:       reg,
		store:       store,
		planner:     pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg, Keys: specKeys},
		custom:      NewCustomSteps(cfg, sp, specKeys, auditRec, gov, log),
		resolver:    resolver,
		rollback:    NewRollbackPolicy(cfg, locators),
		sources:     sources,
		authz:       authzPolicy,
		authzOPA:    cfg.RBAC.Engine == config.RBACOPA && cfg.Auth.Mode != config.AuthModeNone,
		audit:       auditRec,
		governance:  gov,
		tenants:     NewTenantCache(cfg),
		tenantRoles: cfg.RBAC.TenantRoles,
		crossTenant: cfg.RBAC.CrossTenant(),
		secrets:     sp,
		chat:        NewChatNotifier(cfg, sp, log),
		change:      NewChangeProvider(cfg, sp),
		closers:     []io.Closer{auditCloser},
	}, nil
}

// NewWorker starts a DBOS worker that runs deploys, with the service built on its context.
// Used by "serve" and "worker".
func NewWorker(cfg *config.Config) (*App, error) {
	c, err := newComponents(cfg)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(cfg.DBOS.DatabaseURL, "sqlite:") {
		c.log.Warn("database SQLite: supporta worker solo sulla stessa macchina; per più nodi usare Postgres")
	}
	var reg cluster.Registry
	if r, err := cluster.Open(context.Background(), cfg.DBOS.DatabaseURL); err != nil {
		c.log.Warn("registro dei worker non disponibile: niente heartbeat, recovery automatica, retention e freeze a runtime", "error", err)
	} else {
		reg = r
		c.governance.SetStore(r)
	}
	host, _ := os.Hostname()
	rc := cfg.Recovery
	tqueues, listen, err := tenantQueues(cfg, c.tenants, c.log)
	if err != nil {
		return nil, err
	}
	w, err := engine.NewWorker(engine.WorkerOptions{
		AppName:     cfg.App.Name,
		AppVersion:  cfg.App.Version,
		DatabaseURL: cfg.DBOS.DatabaseURL,
		ExecutorID:  cfg.DBOS.ExecutorID,
		Queue: engine.QueueOptions{
			Name:                 cfg.DBOS.Queue,
			WorkerConcurrency:    cfg.DBOS.Concurrency,
			GlobalConcurrency:    cfg.DBOS.GlobalConcurrency,
			PartitionConcurrency: cfg.DBOS.EnvironmentConcurrency,
		},
		Steps:                c.steps,
		Custom:               c.custom,
		Resolver:             c.resolver,
		ResolverRetries:      cfg.EnvConfig.MaxRetries,
		Cleaner:              c.store,
		Notifier:             c.notifier,
		CallbackRetries:      cfg.Callbacks.MaxRetries,
		Chat:                 c.chatNotifier(),
		Change:               c.change,
		ChangePoll:           cfg.Integrations.Change.PollInterval,
		PublicURL:            cfg.Integrations.PublicURL,
		Rollback:             c.rollback,
		Audit:                c.audit,
		Governance:           c.governance,
		PolicyStart:          cfg.Governance.Start,
		Logger:               c.log,
		SchedulePollInterval: cfg.DBOS.SchedulePollInterval,
		Metrics:              recorder(c.metrics),
		Registry:             reg,
		Health:               c.health,
		Traffic:              c.traffic,
		HealthTimeout:        cfg.Rollout.CheckTimeout,
		Inspector:            c.traffic.Deploy,
		Reconcile:            engine.ReconcileOptions{Interval: cfg.Reconcile.Interval, Timeout: cfg.Reconcile.Timeout, Concurrency: cfg.Reconcile.Concurrency},
		Region:               cfg.DBOS.Region,
		RegionRouting:        cfg.DBOS.RegionRouting,
		TenantQueues:         tqueues,
		Listen:               listen,
		ChainPollInterval:    cfg.Chains.PollInterval,
		Build:                version.Version,
		Hostname:             host,
		Workdir:              cfg.WorkDir,
		Maintenance: engine.MaintenanceOptions{
			HeartbeatInterval: rc.HeartbeatInterval, StaleAfter: rc.StaleAfter, RecoverAfter: rc.AutoRecoverAfter,
			Retention:         engine.RetentionOptions{MaxAge: cfg.Retention.MaxAge, KeepReleases: cfg.Retention.KeepReleases},
			RetentionInterval: cfg.Retention.Interval,
		},
	})
	if err != nil {
		if reg != nil {
			_ = reg.Close()
		}
		return nil, err
	}
	eng := c.engine(cfg, w.Client(), w.Admin())
	w.SetService(eng)
	if err := w.Launch(); err != nil {
		if reg != nil {
			_ = reg.Close()
		}
		return nil, fmt.Errorf("starting DBOS: %w", err)
	}
	svc := c.guard(eng)
	stop := c.startGovernance()
	return &App{
		governance: c.governance, stopGovernance: stop, engine: eng,
		Config:   cfg,
		Logger:   c.log,
		Service:  svc,
		slack:    NewSlackInteractions(cfg, c.secrets, svc, c.log),
		worker:   w,
		plugins:  c.plugins,
		closers:  c.closers,
		siem:     c.siem,
		audit:    c.audit,
		metrics:  c.metrics,
		registry: reg,
	}, nil
}

// NewClient creates an application that enqueues and queries deploys without running or
// recovering them: used by CLI commands and by "serve --no-worker". Deploys need at least one running worker.
func NewClient(cfg *config.Config) (*App, error) {
	c, err := newComponents(cfg)
	if err != nil {
		return nil, err
	}
	dbosLog := c.log.With("component", "dbos")
	clientCfg := dbos.ClientConfig{DatabaseURL: cfg.DBOS.DatabaseURL, AppName: cfg.App.Name, Logger: dbosLog}
	client, err := dbos.NewClient(context.Background(), clientCfg)
	if err != nil {
		// The client verifies the DBOS schema but does not create it: on a new database, if the API
		// starts before the workers, it creates it as a worker would and retries. With the schema
		// already there this is not needed: a role without DDL permissions never gets here.
		if merr := engine.Migrate(cfg.DBOS.DatabaseURL, dbosLog); merr != nil {
			return nil, fmt.Errorf("connecting to the DBOS database: %w (schema creation: %v)", err, merr)
		}
		if client, err = dbos.NewClient(context.Background(), clientCfg); err != nil {
			return nil, fmt.Errorf("connecting to the DBOS database: %w", err)
		}
	}
	// The policy store lives in the same database: the API needs it for runtime freezes and
	// policy revisions.
	if store, err := cluster.Open(context.Background(), cfg.DBOS.DatabaseURL); err != nil {
		c.log.Warn("archivio della policy non disponibile: niente freeze a runtime né revisioni della policy", "error", err)
	} else {
		c.governance.SetStore(store)
		c.closers = append(c.closers, store)
	}
	a := &App{
		governance: c.governance, stopGovernance: c.startGovernance(),
		Config:  cfg,
		Logger:  c.log,
		client:  client,
		plugins: c.plugins,
		closers: c.closers,
		siem:    c.siem,
		audit:   c.audit,
		metrics: c.metrics,
	}
	a.engine = c.engine(cfg, client, lazyAdmin{a})
	a.Service = c.guard(a.engine)
	a.slack = NewSlackInteractions(cfg, c.secrets, a.Service, c.log)
	return a, nil
}

// lazyAdmin opens the worker registry only when the worker list is needed.
type lazyAdmin struct{ a *App }

func (l lazyAdmin) Executors(ctx context.Context, staleAfter time.Duration) ([]engine.ExecutorStatus, error) {
	return l.a.Admin().Executors(ctx, staleAfter)
}

func (l lazyAdmin) TargetStates(ctx context.Context) ([]cluster.TargetsRecord, error) {
	return l.a.Admin().TargetStates(ctx)
}

// dbosClient returns the DBOS client of the application (worker context or client).
func (a *App) dbosClient() dbos.Client {
	if a.worker != nil {
		return a.worker.Client()
	}
	return a.client
}

// Admin returns the platform operations (executors, recovery, retention). In client mode it
// opens the worker registry on first use; if it is unavailable, recovery requires --force.
func (a *App) Admin() *engine.Admin {
	a.adminOnce.Do(func() {
		if a.worker != nil {
			a.admin = a.worker.Admin()
			return
		}
		r, err := cluster.Open(context.Background(), a.Config.DBOS.DatabaseURL)
		if err != nil {
			a.Logger.Warn("registro dei worker non disponibile", "error", err)
		} else {
			a.registry = r
		}
		a.admin = engine.NewAdmin(engine.AdminOptions{
			Client: a.client, Registry: a.registry, Version: a.Config.App.Version, Audit: a.audit, Logger: a.Logger,
		})
	})
	return a.admin
}

// Governance returns the governance policy in force (operations outside the permissions, as
// manent-server admin).
func (a *App) Governance() *policy.Runtime { return a.governance }

// Engine returns the service without permissions, for the platform operations.
func (a *App) Engine() *engine.Engine { return a.engine }

// Audit returns the audit recorder.
func (a *App) Audit() audit.Recorder { return a.audit }

// Metrics returns the Prometheus metrics, or nil if disabled.
func (a *App) Metrics() *telemetry.Metrics { return a.metrics }

// EnableStateMetrics adds to the metrics the shared state read from the database: workflows
// per state and queue, executors and heartbeats. Processes that expose /metrics call it.
func (a *App) EnableStateMetrics() error {
	if a.metrics == nil {
		return nil
	}
	return a.metrics.Register(&telemetry.StateCollector{Source: a.Admin(), StaleAfter: a.Config.Recovery.StaleAfter})
}

// Ready checks that the process can serve: it is not stopping, the governance policy is
// loaded and it reaches the database.
func (a *App) Ready(ctx context.Context) error {
	if a.draining.Load() {
		return errors.New("shutting down")
	}
	if a.governance != nil {
		if err := a.governance.Ready(ctx); err != nil {
			return err
		}
	}
	_, err := dbos.ListWorkflows(a.dbosClient(), dbos.WithFilterLimit(1), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	return err
}

// OpsHandler serves /healthz, /readyz and, if enabled, /metrics: it is the
// observability.listen server, used mostly by workers that do not expose the API.
func (a *App) OpsHandler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	api.AddProbes(r, a.Ready)
	if a.metrics != nil {
		r.GET("/metrics", gin.WrapH(a.metrics.Handler()))
	}
	return r
}

// APIOptions are the options of the API router: metrics, /metrics, /readyz and, if enabled,
// the web interface with its configuration.
func (a *App) APIOptions() api.Options {
	o := api.Options{Metrics: recorder(a.metrics), Ready: a.Ready, SlackInteractions: a.slack}
	if a.metrics != nil {
		o.MetricsHandler = a.metrics.Handler()
	}
	if a.Config.HTTP.UI.IsEnabled() {
		ui := NewUIConfig(a.Config)
		o.UIConfig = &ui
		var origins []string
		if ui.Issuer != "" {
			origins = append(origins, ui.Issuer)
		}
		h := webui.New(webui.Options{ConnectOrigins: origins})
		if !h.Built() {
			a.Logger.Warn("interfaccia web non incorporata nel binario: make web e poi make build")
		}
		o.UI = h
	}
	return o
}

// NewUIConfig derives the public configuration of the web interface: login mode and the UI
// OIDC client.
func NewUIConfig(cfg *config.Config) api.UIConfig {
	ui := api.UIConfig{AuthMode: api.UIAuthNone, Version: version.Get().Version}
	if cfg.Auth.Mode == config.AuthModeNone {
		return ui
	}
	o := cfg.Auth.OIDC
	if o == nil || o.UI == nil {
		ui.AuthMode = api.UIAuthUnavailable
		return ui
	}
	ui.AuthMode, ui.Issuer, ui.ClientID = api.UIAuthOIDC, o.Issuer, o.UI.ClientID
	ui.Scopes = o.UI.Scopes
	if len(ui.Scopes) == 0 {
		ui.Scopes = []string{"openid", "profile"}
	}
	ui.RedirectPath = cmp.Or(o.UI.RedirectPath, "/auth/callback")
	return ui
}

// WarnInsecure logs the settings fit only for development. Long-running processes (serve,
// worker) call it, not every CLI command.
func (a *App) WarnInsecure() {
	for _, w := range a.Config.SecurityWarnings() {
		a.Logger.Warn("configurazione non adatta alla produzione", "detail", w)
	}
}

// Close stops the DBOS worker (or client) and closes the SIEM connection. A worker first
// stops dequeuing deploys and waits for the running ones up to dbos.drain_timeout.
func (a *App) Close() {
	var err error
	a.draining.Store(true)
	if a.stopGovernance != nil {
		a.stopGovernance()
	}
	if a.worker != nil {
		a.worker.Drain(a.Config.DBOS.DrainTimeout)
		err = a.worker.Shutdown(30 * time.Second)
	} else {
		err = dbos.Shutdown(a.client, 5*time.Second)
	}
	if err != nil {
		a.Logger.Error("shutdown DBOS", "error", err)
	}
	// Plugins stop after DBOS, when no deploy is using them any more.
	if err := a.plugins.Close(); err != nil {
		a.Logger.Error("arresto plugin", "error", err)
	}
	if a.registry != nil {
		_ = a.registry.Close()
	}
	for _, c := range a.closers {
		_ = c.Close()
	}
	_ = a.siem.Close()
}
