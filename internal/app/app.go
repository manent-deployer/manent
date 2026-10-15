// Package app compone tutti i componenti di manent a partire dalla configurazione.
// È l'unico punto in cui le implementazioni concrete vengono collegate alle interfacce.
package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
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
	"github.com/joeg-ita/manent/internal/engine"
	"github.com/joeg-ita/manent/internal/envconfig"
	"github.com/joeg-ita/manent/internal/extplugin"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/logging"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/rollback"
	"github.com/joeg-ita/manent/internal/rollout"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/steps"
	"github.com/joeg-ita/manent/internal/telemetry"
	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/internal/version"
	"github.com/joeg-ita/manent/internal/webui"
	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/awselb"
	"github.com/joeg-ita/manent/plugins/cloudrun"
	"github.com/joeg-ita/manent/plugins/container"
	"github.com/joeg-ita/manent/plugins/ecs"
	"github.com/joeg-ita/manent/plugins/kubernetes"
	"github.com/joeg-ita/manent/plugins/lambda"
	"github.com/joeg-ita/manent/plugins/remotecopy"
)

// App contiene i componenti condivisi da CLI e API.
type App struct {
	Config *config.Config
	Logger *slog.Logger
	// Service applica i permessi dell'identità nel context (internal/authz).
	Service engine.Service
	plugins *plugin.Registry
	closers []io.Closer
	worker  *engine.Worker // valorizzato in modalità worker
	client  dbos.Client    // valorizzato in modalità client
	siem    logging.SIEMSender
	audit   audit.Recorder
	// metrics è nil con observability.metrics false.
	metrics  *telemetry.Metrics
	registry cluster.Registry
	// admin è creato al primo uso: nei comandi CLI che non lo usano non serve il registro.
	// slack riceve i pulsanti di approvazione dei messaggi Slack (nil se non configurato).
	slack     http.Handler
	adminOnce sync.Once
	admin     *engine.Admin
	draining  atomic.Bool
}

// NewLogger crea il logger applicativo con l'eventuale inoltro al SIEM.
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
	// I log emessi durante uno step finiscono anche nel log del deploy (stream DBOS).
	return slog.New(deploylog.NewHandler(log.Handler())), sender, nil
}

// SetupTracing configura il tracing OpenTelemetry dei processi di lunga durata (serve, worker).
// instance identifica il processo nelle tracce (es. executor_id).
func SetupTracing(ctx context.Context, cfg *config.Config, instance string) (func(context.Context) error, error) {
	t := cfg.Observability.Tracing
	return telemetry.SetupTracing(ctx, telemetry.TracingConfig{
		Enabled: t.Enabled, Endpoint: t.Endpoint, Insecure: t.Insecure, SampleRatio: t.SampleRatio,
		Service: cfg.App.Name, Version: version.Version, Instance: instance,
	})
}

// recorder restituisce il Recorder delle metriche, o uno che le scarta.
func recorder(m *telemetry.Metrics) telemetry.Recorder {
	if m == nil {
		return telemetry.Noop{}
	}
	return m
}

// NewPlugins registra i plugin disponibili e attiva quelli presenti in configurazione.
// I plugin compilati dentro manent vanno aggiunti qui con Register; i plugin esterni
// (chiave "external" in configurazione) sono registrati automaticamente con il loro nome.
func NewPlugins(cfg *config.Config, log *slog.Logger) (*plugin.Registry, error) {
	reg := plugin.NewRegistry()
	for name, f := range map[string]plugin.Factory{
		remotecopy.Name: remotecopy.Factory,
		container.Name:  container.Factory,
		kubernetes.Name: kubernetes.Factory,
		ecs.Name:        ecs.Factory,
		lambda.Name:     lambda.Factory,
		cloudrun.Name:   cloudrun.Factory,
	} {
		if err := reg.Register(name, f); err != nil {
			return nil, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Plugins)) {
		pc := cfg.Plugins[name]
		if extplugin.IsExternal(pc) {
			if err := reg.Register(name, extplugin.NewFactory(name)); err != nil {
				return nil, fmt.Errorf("plugin esterno: %w", err)
			}
		}
		if err := reg.Activate(name, pc, log); err != nil {
			_ = reg.Close()
			return nil, err
		}
	}
	return reg, nil
}

// NewLocators restituisce le strategie con cui individuare la release a cui tornare in caso
// di rollback, per nome (rollback.locator). Un locator che interroga un sistema aziendale dei
// rilasci in esercizio va aggiunto qui, implementando plugin.ReleaseLocator.
func NewLocators(cfg *config.Config, log *slog.Logger) map[string]plugin.ReleaseLocator {
	return map[string]plugin.ReleaseLocator{
		plugin.PluginLocatorName: plugin.PluginLocator{},
	}
}

// NewRollbackPolicy crea la policy di rollback dalla configurazione e dai locator registrati.
func NewRollbackPolicy(cfg *config.Config, locators map[string]plugin.ReleaseLocator) rollback.Policy {
	return rollback.NewPolicy(cfg, slices.Sorted(maps.Keys(locators)))
}

// NewSteps crea il registry degli step standard. Lo store degli artefatti è condiviso
// tra lo step artifacts, lo step deploy (che lo riscarica se manca) e la pulizia finale.
// Restituisce anche le operazioni su load balancer e router usate dal motore (svuotamento dei
// lotti e rilasci per percentuale), che condividono il plugin di deploy e i segreti.
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

// NewTrafficProviders restituisce i provider di load balancer e router: quelli HTTP e SSH di
// internal/traffic e quelli basati sugli SDK dei cloud.
func NewTrafficProviders() *traffic.Providers {
	p := traffic.Default()
	p.Pools["aws-elb"] = traffic.Provider[traffic.PoolFactory]{New: awselb.NewPool(awselb.SDKConnector{}), Validate: awselb.ValidatePool}
	return p
}

// NewSecretProvider crea il secret provider con gli schemi env:, file: e, se abilitato, vault:.
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

// NewArtifactSources crea le sorgenti di artefatti configurate, con le relative credenziali.
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

// NewAuthenticator crea l'autenticazione dell'API: token statici e, se configurato, OIDC
// (con la discovery dell'issuer). Restituisce nil con auth.mode none.
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
		})
		if err != nil {
			return nil, err
		}
		chain = append(chain, v)
	}
	return chain, nil
}

// NewAuthzPolicy crea la policy RBAC; con auth.mode none concede tutto.
func NewAuthzPolicy(cfg *config.Config) (authz.Policy, error) {
	if cfg.Auth.Mode == config.AuthModeNone {
		return authz.Policy{Disabled: true}, nil
	}
	roles := map[string][]authz.Action{}
	for name, acts := range cfg.RBAC.Roles {
		for _, a := range acts {
			roles[name] = append(roles[name], authz.Action(a))
		}
	}
	bindings := make([]authz.Binding, len(cfg.RBAC.Bindings))
	for i, b := range cfg.RBAC.Bindings {
		bindings[i] = authz.Binding{Subjects: b.Subjects, Groups: b.Groups, Role: b.Role, Applications: b.Applications, Environments: b.Environments}
	}
	return authz.NewPolicy(roles, bindings)
}

// NewAuditRecorder crea il registro di audit: log applicativo (e SIEM) e, se configurato, file
// con catena di hash (HMAC se è indicata la chiave).
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

// AuditKey legge la chiave HMAC della catena di audit, se configurata.
func AuditKey(cfg *config.Config, sp secrets.Provider) ([]byte, error) {
	if cfg.Audit.HMACKeyRef == "" {
		return nil, nil
	}
	v, err := sp.Resolve(context.Background(), cfg.Audit.HMACKeyRef)
	if err != nil {
		return nil, fmt.Errorf("audit.hmac_key_ref: %w", err)
	}
	if len(v) < 16 {
		return nil, errors.New("audit.hmac_key_ref: chiave troppo corta (almeno 16 byte)")
	}
	return []byte(v), nil
}

// NewGovernance crea le regole di rilascio e le finestre di freeze dalla configurazione.
func NewGovernance(cfg *config.Config) (*governance.Policy, error) {
	g := cfg.Governance
	rules := make([]governance.Rule, len(g.Rules))
	for i, r := range g.Rules {
		rules[i] = governance.Rule{Name: r.Name, Applications: r.Applications, Environments: r.Environments, Requires: r.Requires}
		if a := r.Approvals; a != nil {
			rules[i].Approvals = governance.Approvals{Required: a.Required, AllowSelf: a.AllowSelf, Timeout: a.Timeout}
		}
		if ch := r.Change; ch != nil {
			rules[i].Change = &governance.Change{Approval: ch.Approval, Timeout: cmp.Or(ch.Timeout, 72*time.Hour)}
		}
	}
	windows := make([]governance.Window, len(g.Freezes))
	for i, f := range g.Freezes {
		windows[i] = governance.Window{
			Name: f.Name, Reason: f.Reason, Applications: f.Applications, Environments: f.Environments, Regions: f.Regions,
			From: f.From, To: f.To, Cron: f.Cron, Duration: f.Duration, Timezone: f.Timezone,
		}
	}
	return governance.New(rules, windows)
}

// NewHealthChecker crea l'health gate dei rollout a ondate: sonde HTTP, Prometheus e
// Alertmanager. Un sistema di metriche diverso si aggiunge qui, implementando rollout.Checker.
func NewHealthChecker(cfg *config.Config) rollout.Checker {
	return &rollout.HTTPChecker{
		Client:          &http.Client{Timeout: cfg.Rollout.CheckTimeout},
		PrometheusURL:   cfg.Rollout.PrometheusURL,
		AlertmanagerURL: cfg.Rollout.AlertmanagerURL,
	}
}

// NewDescriptorPolicy restituisce la policy sui descrittori della configurazione.
func NewDescriptorPolicy(cfg *config.Config) *envconfig.DescriptorPolicy {
	dp := cfg.DescriptorPolicy
	return &envconfig.DescriptorPolicy{Params: dp.Params, SecretRefs: dp.SecretRefs, Targets: dp.Targets == nil || *dp.Targets, StepParams: dp.StepParams}
}

// NewResolver crea il resolver della configurazione di ambiente in base al provider configurato.
func NewResolver(cfg *config.Config) (envconfig.Resolver, error) {
	ec := cfg.EnvConfig
	r := envconfig.Resolver{Required: ec.Required, Policy: NewDescriptorPolicy(cfg)}
	switch ec.Provider {
	case config.EnvProviderConsul:
		addr := ec.Consul.Address
		if addr == "" {
			addr = cmp.Or(os.Getenv("CONSUL_HTTP_ADDR"), "http://127.0.0.1:8500")
		}
		if !strings.Contains(addr, "://") {
			addr = "http://" + addr
		}
		token := os.Getenv("CONSUL_HTTP_TOKEN")
		if ec.Consul.TokenFile != "" {
			b, err := os.ReadFile(ec.Consul.TokenFile)
			if err != nil {
				return r, fmt.Errorf("lettura token Consul: %w", err)
			}
			token = strings.TrimSpace(string(b))
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
	}
	return r, nil
}

// NewCallbackPolicy restituisce i limiti sugli URL dei callback definiti in configurazione.
func NewCallbackPolicy(cfg *config.Config) callback.Policy {
	return callback.Policy{AllowHTTP: cfg.Callbacks.AllowHTTP, AllowedHosts: cfg.Callbacks.AllowedHosts}
}

// components contiene i componenti comuni a worker e client.
type components struct {
	log        *slog.Logger
	plugins    *plugin.Registry
	siem       logging.SIEMSender
	steps      *pipeline.Registry
	store      *artifact.Store
	planner    pipeline.Planner
	resolver   envconfig.Resolver
	policy     callback.Policy
	notifier   callback.Notifier
	rollback   rollback.Policy
	sources    *artifact.Sources
	authz      authz.Policy
	audit      audit.Recorder
	governance *governance.Policy
	custom     *customstep.Builder
	secrets    secrets.Provider
	chat       *chat.Notifier
	change     change.Provider
	health     rollout.Checker
	traffic    *steps.Traffic
	metrics    *telemetry.Metrics
	closers    []io.Closer
}

// service costruisce il servizio esposto a CLI e API: l'Engine avvolto dai permessi.
// executors fornisce l'elenco dei worker (Admin, aperto al primo uso in modalità client).
func (c *components) service(cfg *config.Config, client dbos.Client, executors engine.ExecutorLister) engine.Service {
	return c.guard(c.engine(cfg, client, executors))
}

// engine costruisce l'Engine senza permessi: il worker lo usa per i rollback delle release,
// già autorizzati all'invio.
func (c *components) engine(cfg *config.Config, client dbos.Client, executors engine.ExecutorLister) *engine.Engine {
	opts := engine.ServiceOptions{
		Client: client, QueueName: cfg.DBOS.Queue, Planner: c.planner,
		CallbackPolicy: c.policy, Rollback: c.rollback, Sources: c.sources,
		Resolver: c.resolver, Governance: c.governance, Health: c.health, Traffic: c.traffic, Logger: c.log,
		Executors: executors, StaleAfter: cfg.Recovery.StaleAfter,
	}
	if c.chat != nil {
		opts.Notifications = c.chat
	}
	return engine.NewService(opts)
}

// chatNotifier restituisce il notificatore come interfaccia, nil senza destinazioni.
func (c *components) chatNotifier() engine.ChatNotifier {
	if c.chat == nil {
		return nil
	}
	return c.chat
}

// guard avvolge l'Engine con i permessi.
func (c *components) guard(eng *engine.Engine) engine.Service {
	return &authz.Guard{Next: eng, Policy: c.authz, Audit: c.audit, Logger: c.log.With("component", "authz")}
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
	resolver, err := NewResolver(cfg)
	if err != nil {
		return nil, err
	}
	sp, err := NewSecretProvider(cfg)
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
	gov, err := NewGovernance(cfg)
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
	return &components{
		metrics:    metrics,
		health:     NewHealthChecker(cfg),
		traffic:    trafficOps,
		plugins:    plugins,
		policy:     policy,
		notifier:   notifier,
		log:        log,
		siem:       sender,
		steps:      reg,
		store:      store,
		planner:    pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg, Keys: specKeys},
		custom:     NewCustomSteps(cfg, sp, specKeys, auditRec, gov, log),
		resolver:   resolver,
		rollback:   NewRollbackPolicy(cfg, locators),
		sources:    sources,
		authz:      authzPolicy,
		audit:      auditRec,
		governance: gov,
		secrets:    sp,
		chat:       NewChatNotifier(cfg, sp, log),
		change:     NewChangeProvider(cfg, sp),
		closers:    []io.Closer{auditCloser},
	}, nil
}

// NewWorker avvia un worker DBOS che esegue i deploy, con il servizio costruito sul suo contesto.
// Usato da "serve" e "worker".
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
		c.log.Warn("registro dei worker non disponibile: niente heartbeat, recovery automatica e retention", "error", err)
	} else {
		reg = r
	}
	host, _ := os.Hostname()
	rc := cfg.Recovery
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
		Logger:               c.log,
		SchedulePollInterval: cfg.DBOS.SchedulePollInterval,
		Metrics:              recorder(c.metrics),
		Registry:             reg,
		Health:               c.health,
		Traffic:              c.traffic,
		HealthTimeout:        cfg.Rollout.CheckTimeout,
		Region:               cfg.DBOS.Region,
		RegionRouting:        cfg.DBOS.RegionRouting,
		ChainPollInterval:    cfg.Chains.PollInterval,
		Build:                version.Version,
		Hostname:             host,
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
		return nil, fmt.Errorf("avvio DBOS: %w", err)
	}
	svc := c.guard(eng)
	return &App{
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

// NewClient crea un'applicazione che accoda e interroga i deploy senza eseguirli né
// recuperarli: usata dai comandi CLI e da "serve --no-worker". I deploy richiedono almeno un worker attivo.
func NewClient(cfg *config.Config) (*App, error) {
	c, err := newComponents(cfg)
	if err != nil {
		return nil, err
	}
	client, err := dbos.NewClient(context.Background(), dbos.ClientConfig{
		DatabaseURL: cfg.DBOS.DatabaseURL,
		AppName:     cfg.App.Name,
		Logger:      c.log.With("component", "dbos"),
	})
	if err != nil {
		return nil, fmt.Errorf("connessione al database DBOS: %w", err)
	}
	a := &App{
		Config:  cfg,
		Logger:  c.log,
		client:  client,
		plugins: c.plugins,
		closers: c.closers,
		siem:    c.siem,
		audit:   c.audit,
		metrics: c.metrics,
	}
	a.Service = c.service(cfg, client, lazyAdmin{a})
	a.slack = NewSlackInteractions(cfg, c.secrets, a.Service, c.log)
	return a, nil
}

// lazyAdmin apre il registro dei worker solo quando serve l'elenco dei worker.
type lazyAdmin struct{ a *App }

func (l lazyAdmin) Executors(ctx context.Context, staleAfter time.Duration) ([]engine.ExecutorStatus, error) {
	return l.a.Admin().Executors(ctx, staleAfter)
}

// dbosClient restituisce il client DBOS dell'applicazione (contesto del worker o client).
func (a *App) dbosClient() dbos.Client {
	if a.worker != nil {
		return a.worker.Client()
	}
	return a.client
}

// Admin restituisce le operazioni di piattaforma (executor, recovery, retention). In modalità
// client apre il registro dei worker al primo uso; se non è disponibile, la recovery richiede
// --force.
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

// Metrics restituisce le metriche Prometheus, o nil se disattivate.
func (a *App) Metrics() *telemetry.Metrics { return a.metrics }

// EnableStateMetrics aggiunge alle metriche lo stato condiviso letto dal database: workflow
// per stato e coda, executor e heartbeat. Va chiamato dai processi che espongono /metrics.
func (a *App) EnableStateMetrics() error {
	if a.metrics == nil {
		return nil
	}
	return a.metrics.Register(&telemetry.StateCollector{Source: a.Admin(), StaleAfter: a.Config.Recovery.StaleAfter})
}

// Ready verifica che il processo possa servire: non è in arresto e raggiunge il database.
func (a *App) Ready(context.Context) error {
	if a.draining.Load() {
		return errors.New("in arresto")
	}
	_, err := dbos.ListWorkflows(a.dbosClient(), dbos.WithFilterLimit(1), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	return err
}

// OpsHandler serve /healthz, /readyz e, se abilitate, /metrics: è il server di
// observability.listen, usato soprattutto dai worker che non espongono l'API.
func (a *App) OpsHandler() http.Handler {
	r := gin.New()
	r.Use(gin.Recovery())
	api.AddProbes(r, a.Ready)
	if a.metrics != nil {
		r.GET("/metrics", gin.WrapH(a.metrics.Handler()))
	}
	return r
}

// APIOptions sono le opzioni del router dell'API: metriche, /metrics, /readyz e, se attiva,
// l'interfaccia web con la sua configurazione.
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

// NewUIConfig ricava la configurazione pubblica dell'interfaccia web: modalità di login e
// client OIDC della UI.
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

// WarnInsecure registra a log le impostazioni adatte solo allo sviluppo. Va chiamato dai
// processi di lunga durata (serve, worker), non da ogni comando CLI.
func (a *App) WarnInsecure() {
	for _, w := range a.Config.SecurityWarnings() {
		a.Logger.Warn("configurazione non adatta alla produzione", "detail", w)
	}
}

// Close arresta il worker (o il client) DBOS e chiude la connessione al SIEM. Un worker prima
// smette di prelevare deploy e attende quelli in corso fino a dbos.drain_timeout.
func (a *App) Close() {
	var err error
	a.draining.Store(true)
	if a.worker != nil {
		a.worker.Drain(a.Config.DBOS.DrainTimeout)
		err = a.worker.Shutdown(30 * time.Second)
	} else {
		err = dbos.Shutdown(a.client, 5*time.Second)
	}
	if err != nil {
		a.Logger.Error("shutdown DBOS", "error", err)
	}
	// I plugin si arrestano dopo DBOS, quando nessun deploy li sta più usando.
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
