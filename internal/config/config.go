// Package config handles manent's static application configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/customspec"
)

// Modes of a pipeline step.
const (
	ModeMandatory = "mandatory"
	ModeOptional  = "optional"
)

// Config is the complete application configuration.
type Config struct {
	App       App       `yaml:"app"`
	DBOS      DBOS      `yaml:"dbos"`
	HTTP      HTTP      `yaml:"http"`
	Logging   Logging   `yaml:"logging"`
	SIEM      SIEM      `yaml:"siem"`
	WorkDir   string    `yaml:"workdir"`
	EnvConfig EnvConfig `yaml:"envconfig"`
	Callbacks Callbacks `yaml:"callbacks"`
	Rollback  Rollback  `yaml:"rollback"`
	Auth      Auth      `yaml:"auth"`
	RBAC      RBAC      `yaml:"rbac"`
	Audit     Audit     `yaml:"audit"`
	// DescriptorPolicy limits what descriptors can add to the environment configuration.
	DescriptorPolicy DescriptorPolicy          `yaml:"descriptor_policy"`
	Secrets          Secrets                   `yaml:"secrets"`
	Governance       Governance                `yaml:"governance"`
	Artifacts        Artifacts                 `yaml:"artifacts"`
	Pipeline         Pipeline                  `yaml:"pipeline"`
	Plugins          map[string]map[string]any `yaml:"plugins"`
	Observability    Observability             `yaml:"observability"`
	Rollout          RolloutConfig             `yaml:"rollout"`
	Chains           Chains                    `yaml:"chains"`
	Recovery         Recovery                  `yaml:"recovery"`
	Retention        Retention                 `yaml:"retention"`
	Integrations     Integrations              `yaml:"integrations"`
}

// Observability configures metrics, tracing and probes of long-running processes.
type Observability struct {
	// Listen, if set, starts a server with /metrics, /healthz and /readyz on this address: it is
	// for the worker, which has no API. With serve the same routes are also on the API address.
	Listen string `yaml:"listen"`
	// Metrics enables Prometheus metrics.
	Metrics bool    `yaml:"metrics"`
	Tracing Tracing `yaml:"tracing"`
}

// Tracing configures the export of OpenTelemetry traces over OTLP/HTTP.
type Tracing struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint of the collector (e.g. otel-collector:4318); empty: OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint"`
	// Insecure uses http instead of https.
	Insecure bool `yaml:"insecure"`
	// SampleRatio is the fraction of sampled traces (default 1).
	SampleRatio float64 `yaml:"sample_ratio"`
}

// RolloutConfig configures the health gate of wave rollouts.
type RolloutConfig struct {
	// PrometheusURL is the Prometheus API for gate metrics (rollout.health.metrics).
	PrometheusURL string `yaml:"prometheus_url"`
	// AlertmanagerURL is the Alertmanager API for alerts (rollout.health.alerts).
	AlertmanagerURL string `yaml:"alertmanager_url"`
	// CheckTimeout limits every health gate read.
	CheckTimeout time.Duration `yaml:"check_timeout"`
}

// Chains configures dependencies between deploys (after) and composite releases.
type Chains struct {
	// PollInterval is the first interval for reading the state of awaited deploys; it doubles at
	// every read up to 1 minute.
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Recovery configures the worker registry and the resumption of a lost worker's workflows.
type Recovery struct {
	// HeartbeatInterval is how often a worker updates the registry.
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	// StaleAfter is the heartbeat delay beyond which a worker is considered lost.
	StaleAfter time.Duration `yaml:"stale_after"`
	// AutoRecoverAfter, if > 0, makes another worker resume the pending workflows of a worker of
	// the same version that stopped or sent no heartbeat for this long. 0 = manual only
	// (manent admin recover).
	AutoRecoverAfter time.Duration `yaml:"auto_recover_after"`
}

// Retention configures the cleanup of the workflow history.
type Retention struct {
	// MaxAge: workflows ended more than MaxAge ago are deleted (0 = never).
	MaxAge time.Duration `yaml:"max_age"`
	// KeepReleases are the successful deploys always kept per application and environment.
	KeepReleases int `yaml:"keep_releases"`
	// Interval is how often a worker runs the cleanup.
	Interval time.Duration `yaml:"interval"`
}

// App identifies the application.
type App struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// DBOS configures the workflow engine and the deploy queue.
type DBOS struct {
	DatabaseURL string `yaml:"database_url"`
	// ExecutorID identifies the worker: it must be unique across nodes and stable across
	// restarts, because at startup a worker only recovers the pending workflows with its own ID.
	// Default: hostname. The DBOS__VMID variable takes precedence.
	ExecutorID string `yaml:"executor_id"`
	Queue      string `yaml:"queue"`
	// Concurrency is the maximum number of deploys running on each worker.
	Concurrency int `yaml:"concurrency"`
	// GlobalConcurrency is the maximum number of deploys running on all workers (0 = no limit).
	GlobalConcurrency int `yaml:"global_concurrency"`
	// EnvironmentConcurrency is the maximum number of concurrent deploys of the same
	// application/environment on all workers.
	EnvironmentConcurrency int `yaml:"environment_concurrency"`
	// SchedulePollInterval is how often schedule changes are read again from the database.
	SchedulePollInterval time.Duration `yaml:"schedule_poll_interval"`
	// Region is the worker's region: with RegionRouting it runs the wave rollout passes on the
	// targets of this region (queue <queue>.<region>).
	Region string `yaml:"region"`
	// RegionRouting makes the workers of the targets' region run the rollout passes, as child
	// workflows; without it, the orchestrator runs them inline.
	RegionRouting bool `yaml:"region_routing"`
	// DrainTimeout is how long a stopping worker waits for the running deploys after it stopped
	// dequeuing (0 = no wait).
	DrainTimeout time.Duration `yaml:"drain_timeout"`
}

// HTTP configures the API web server.
type HTTP struct {
	Listen string `yaml:"listen"`
	UI     UI     `yaml:"ui"`
}

// UI configures the web interface served by the API.
type UI struct {
	// Enabled serves the interface on / (default true).
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled reports whether the web interface is enabled (default true).
func (u UI) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// Logging configures the application log.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// SIEM configures sending structured logs: to an RFC 5424 collector (udp, tcp), to the local
// syslog socket (unixgram, e.g. /dev/log) or to journald with the native protocol (journald).
type SIEM struct {
	Enabled bool   `yaml:"enabled"`
	Network string `yaml:"network"`
	Address string `yaml:"address"`
	// Destinations are destinations besides network/address (e.g. the remote SIEM and the local
	// journal together).
	Destinations []SIEMDestination `yaml:"destinations"`
}

// SIEMDestination is a structured log destination. For unixgram and journald, address is the
// socket path (default /dev/log and /run/systemd/journal/socket).
type SIEMDestination struct {
	Network string `yaml:"network"`
	Address string `yaml:"address"`
}

// Targets returns the active destinations: none if sending is disabled.
func (s SIEM) Targets() []SIEMDestination {
	if !s.Enabled {
		return nil
	}
	out := []SIEMDestination{}
	if s.Network != "" && (s.Address != "" || s.Network == "unixgram" || s.Network == "journald") {
		out = append(out, SIEMDestination{Network: s.Network, Address: s.Address})
	}
	return append(out, s.Destinations...)
}

// Supported environment configuration providers.
const (
	EnvProviderNone   = "none"
	EnvProviderConsul = "consul"
	EnvProviderFile   = "file"
)

// EnvConfig configures the system the deploy configuration of every environment is read from.
type EnvConfig struct {
	// Provider is none, consul or file.
	Provider string `yaml:"provider"`
	// Required fails the deploy if the environment has no configuration.
	Required bool `yaml:"required"`
	// MaxRetries are the extra attempts on a read error.
	MaxRetries int          `yaml:"max_retries"`
	Consul     ConsulConfig `yaml:"consul"`
	File       FileConfig   `yaml:"file"`
}

// ConsulConfig configures reading from the Consul KV store.
// The ACL token must not be written in the file: it is read from CONSUL_HTTP_TOKEN or token_file.
type ConsulConfig struct {
	// Address of the agent (default CONSUL_HTTP_ADDR or http://127.0.0.1:8500).
	Address    string `yaml:"address"`
	Datacenter string `yaml:"datacenter"`
	// Key is the key template, with {application} and {environment}.
	Key       string `yaml:"key"`
	TokenFile string `yaml:"token_file"`
}

// FileConfig configures reading from local files.
type FileConfig struct {
	// Path is the path template, with {application} and {environment}.
	Path string `yaml:"path"`
}

// Callbacks configures sending events to the callbacks named in descriptors.
type Callbacks struct {
	// Timeout of every call.
	Timeout time.Duration `yaml:"timeout"`
	// MaxRetries are the extra attempts of every send.
	MaxRetries int `yaml:"max_retries"`
	// AllowHTTP allows http callbacks; by default https only.
	AllowHTTP bool `yaml:"allow_http"`
	// AllowedHosts limits the callback hosts (also "*.domain"); empty = all.
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// Rollback configures the automatic rollback policies.
type Rollback struct {
	// Locator is the default strategy to find the release to go back to.
	Locator string `yaml:"locator"`
	// ArtifactTypes is the policy per artifact type. Unlisted types do not allow rollback.
	ArtifactTypes map[string]RollbackType `yaml:"artifact_types"`
}

// RollbackType is the rollback policy of an artifact type.
type RollbackType struct {
	// Allowed reports whether rollback is possible for this type (e.g. not for database migrations).
	Allowed bool `yaml:"allowed"`
	// Enabled is the default, which descriptor and environment configuration can override.
	Enabled bool `yaml:"enabled"`
	// On lists the steps whose failure starts the rollback (default: DefaultRollbackOn).
	On []string `yaml:"on"`
	// Locator overrides the default locator for this type.
	Locator string `yaml:"locator"`
}

// DefaultRollbackOn are the steps that start the rollback by default: those after the first
// change to the environments.
var DefaultRollbackOn = []string{"deploy", "post-test", "activation"}

// Authentication modes.
const (
	// AuthModeNone disables authentication and permissions: development only.
	AuthModeNone    = "none"
	AuthModeEnabled = "enabled"
)

// Auth configures the API authentication.
type Auth struct {
	// Mode is none (default) or enabled.
	Mode   string     `yaml:"mode"`
	OIDC   *OIDC      `yaml:"oidc"`
	Tokens []APIToken `yaml:"tokens"`
	// TokensFile is a YAML file with more tokens (tokens key), reloaded when it changes: tokens
	// can be added and revoked without restarting.
	TokensFile string `yaml:"tokens_file"`
}

// OIDC configures the verification of tokens of an OIDC identity provider.
type OIDC struct {
	Issuer        string `yaml:"issuer"`
	Audience      string `yaml:"audience"`
	UsernameClaim string `yaml:"username_claim"`
	GroupsClaim   string `yaml:"groups_claim"`
	CAFile        string `yaml:"ca_file"`
	// UI is the public OIDC client (Authorization Code + PKCE) of the web interface: its access
	// tokens must have the audience and the groups claim verified above.
	UI *OIDCUI `yaml:"ui"`
}

// OIDCUI is the OIDC client of the web interface.
type OIDCUI struct {
	ClientID string   `yaml:"client_id"`
	Scopes   []string `yaml:"scopes"`
	// RedirectPath is the return path after login (default /auth/callback).
	RedirectPath string `yaml:"redirect_path"`
}

// APIToken is a static token for automated systems: the configuration holds only its sha256.
type APIToken struct {
	Name   string   `yaml:"name"`
	SHA256 string   `yaml:"sha256"`
	Groups []string `yaml:"groups"`
	// ExpiresAt, if set, is the token expiry.
	ExpiresAt time.Time `yaml:"expires_at"`
}

// RBAC assigns roles per application and environment.
type RBAC struct {
	// Roles defines roles in addition to the predefined ones (viewer, tester, deployer, operator, admin).
	Roles    map[string][]string `yaml:"roles"`
	Bindings []RBACBinding       `yaml:"bindings"`
}

// RBACBinding assigns a role to users or groups on applications and environments ("*" patterns).
type RBACBinding struct {
	Subjects     []string `yaml:"subjects"`
	Groups       []string `yaml:"groups"`
	Role         string   `yaml:"role"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
}

// Audit configures the recording of actions.
type Audit struct {
	// File, if set, receives the audit entries as JSON Lines (besides the log and the SIEM),
	// chained by a hash that makes changes and deletions detectable.
	File string `yaml:"file"`
	// MaxSizeMB is the size beyond which the file is rotated (0 = no rotation).
	MaxSizeMB int `yaml:"max_size_mb"`
	// HMACKeyRef references a secret key: with the key the chain is an HMAC and cannot be
	// recomputed by whoever edits the file.
	HMACKeyRef string `yaml:"hmac_key_ref"`
}

// Governance configures the release rules: promotion, approvals and freezes.
type Governance struct {
	// Rules apply per application and environment: the first match wins.
	Rules   []GovernanceRule `yaml:"rules"`
	Freezes []FreezeWindow   `yaml:"freezes"`
}

// GovernanceRule are the release rules of applications and environments ("*" patterns).
type GovernanceRule struct {
	Name         string   `yaml:"name"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
	// Requires lists the environments where the same artifact must already have been released.
	Requires  []string          `yaml:"requires"`
	Approvals *ApprovalSettings `yaml:"approvals"`
	// Change opens a change management ticket for every deploy (integrations.change).
	Change *ChangeRule `yaml:"change"`
}

// ApprovalSettings configures the approvals required before the deploy.
type ApprovalSettings struct {
	Required  int           `yaml:"required"`
	AllowSelf bool          `yaml:"allow_self"`
	Timeout   time.Duration `yaml:"timeout"`
	// When: before (default) or rollout, to ask for them at the rollout's approval points.
	When string `yaml:"when"`
}

// FreezeWindow is a window in which deploys are blocked: absolute (from/to) or recurring
// (5-field cron for duration).
type FreezeWindow struct {
	Name         string        `yaml:"name"`
	Reason       string        `yaml:"reason"`
	Applications []string      `yaml:"applications"`
	Environments []string      `yaml:"environments"`
	Regions      []string      `yaml:"regions"`
	From         time.Time     `yaml:"from"`
	To           time.Time     `yaml:"to"`
	Cron         string        `yaml:"cron"`
	Duration     time.Duration `yaml:"duration"`
	Timezone     string        `yaml:"timezone"`
}

// DescriptorPolicy limits what a descriptor can add to the environment configuration.
// Missing fields mean "no limit", for compatibility: set them in production.
type DescriptorPolicy struct {
	// Params lists per plugin ("*" = all) the patterns of the parameters a descriptor can set.
	Params map[string][]string `yaml:"params"`
	// SecretRefs are the patterns of the secret references a descriptor can declare, with
	// {application} and {environment}.
	SecretRefs []string `yaml:"secret_refs"`
	// Targets allows targets given by the descriptor.
	Targets *bool `yaml:"targets"`
	// StepParams lists per custom step the patterns of the parameters a descriptor can set.
	// Default: none, parameters come from the environment configuration.
	StepParams map[string][]string `yaml:"step_params"`
}

// Secrets configures the secret providers besides env: and file:.
type Secrets struct {
	Vault Vault `yaml:"vault"`
}

// Vault configures the "vault:" scheme.
type Vault struct {
	Enabled bool `yaml:"enabled"`
	// Address defaults to VAULT_ADDR.
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
	// Auth is token (default), approle or kubernetes.
	Auth       string        `yaml:"auth"`
	TokenFile  string        `yaml:"token_file"`
	AppRole    VaultAppRole  `yaml:"approle"`
	Kubernetes VaultK8s      `yaml:"kubernetes"`
	CAFile     string        `yaml:"ca_file"`
	Timeout    time.Duration `yaml:"timeout"`
}

// VaultAppRole configures the AppRole login.
type VaultAppRole struct {
	RoleID       string `yaml:"role_id"`
	SecretIDFile string `yaml:"secret_id_file"`
	Mount        string `yaml:"mount"`
}

// VaultK8s configures the login with the Kubernetes service account.
type VaultK8s struct {
	Role    string `yaml:"role"`
	JWTFile string `yaml:"jwt_file"`
	Mount   string `yaml:"mount"`
}

// Artifacts configures artifact downloads.
type Artifacts struct {
	// Timeout of a download (default 30m).
	Timeout time.Duration `yaml:"timeout"`
	// AllowUnlisted allows sources not listed in Sources (default true).
	AllowUnlisted *bool            `yaml:"allow_unlisted"`
	Sources       []ArtifactSource `yaml:"sources"`
}

// ArtifactSource is an artifact source, with optional credentials.
type ArtifactSource struct {
	// Prefix of the URL (https://, http:// or file:///).
	Prefix string        `yaml:"prefix"`
	Auth   *ArtifactAuth `yaml:"auth"`
	CAFile string        `yaml:"ca_file"`
}

// ArtifactAuth are the credentials of a source: SecretRef is a secret provider reference
// (e.g. vault:...), never the value.
type ArtifactAuth struct {
	Type      string `yaml:"type"`
	Username  string `yaml:"username"`
	Header    string `yaml:"header"`
	SecretRef string `yaml:"secret_ref"`
}

// Pipeline lists the available steps in execution order.
type Pipeline struct {
	Steps []Step `yaml:"steps"`
	// Custom configures custom steps (steps with type).
	Custom CustomSteps `yaml:"custom"`
}

// Step defines the static configuration of a step.
type Step struct {
	Name string `yaml:"name"`
	Mode string `yaml:"mode"`
	// Enabled is the default for optional steps, which the descriptor can override.
	Enabled    bool `yaml:"enabled"`
	MaxRetries int  `yaml:"max_retries"`
	// Spec, with type, defines a custom step (internal/customspec).
	customspec.Spec `yaml:",inline"`
}

// Default returns a working configuration for local development.
func Default() *Config {
	return &Config{
		App: App{Name: "manent", Version: "0.1.0"},
		DBOS: DBOS{
			DatabaseURL: "sqlite:manent.db", Queue: "deployments", Concurrency: 4, EnvironmentConcurrency: 1,
			DrainTimeout: 2 * time.Minute,
		},
		HTTP:    HTTP{Listen: ":8080"},
		Logging: Logging{Level: "info", Format: "text"},
		SIEM:    SIEM{Network: "udp"},
		WorkDir: "./work",
		EnvConfig: EnvConfig{
			Provider:   EnvProviderNone,
			MaxRetries: 3,
			Consul:     ConsulConfig{Key: "manent/{application}/{environment}"},
			File:       FileConfig{Path: "./envconfig/{application}/{environment}.yml"},
		},
		Callbacks: Callbacks{Timeout: 10 * time.Second, MaxRetries: 3},
		// Rollback is allowed but disabled for applications; never for migrations, which usually
		// cannot be reversed.
		Rollback: Rollback{Locator: "plugin", ArtifactTypes: map[string]RollbackType{
			"application": {Allowed: true},
			"container":   {Allowed: true},
			"kubernetes":  {Allowed: true},
			"ecs":         {Allowed: true},
			"lambda":      {Allowed: true},
			"cloudrun":    {Allowed: true},
			"war":         {Allowed: true},
			"ear":         {Allowed: true},
			"migration":   {Allowed: false},
		}},
		Pipeline: Pipeline{Steps: []Step{
			{Name: "secrets", Mode: ModeMandatory},
			{Name: "artifacts", Mode: ModeMandatory, MaxRetries: 3},
			{Name: "qa", Mode: ModeOptional, Enabled: true},
			{Name: "deploy", Mode: ModeMandatory, MaxRetries: 2},
			{Name: "post-test", Mode: ModeOptional, Enabled: false, MaxRetries: 5},
			{Name: "activation", Mode: ModeOptional, Enabled: true},
		}},
		Plugins:       map[string]map[string]any{"remote-copy": {}},
		Auth:          Auth{Mode: AuthModeNone},
		Artifacts:     Artifacts{Timeout: 30 * time.Minute},
		Observability: Observability{Metrics: true, Tracing: Tracing{SampleRatio: 1}},
		Recovery:      Recovery{HeartbeatInterval: 15 * time.Second, StaleAfter: time.Minute},
		Retention:     Retention{KeepReleases: 10, Interval: time.Hour},
		Rollout:       RolloutConfig{CheckTimeout: 30 * time.Second},
		Chains:        Chains{PollInterval: 5 * time.Second},
	}
}

// applyDefaults completes the sections whose default cannot live in Default(): YAML would
// merge the configured maps with the default ones instead of replacing them.
func (c *Config) applyDefaults() {
	dp := &c.DescriptorPolicy
	if dp.Params == nil {
		dp.Params = map[string][]string{"*": {"*"}}
	}
	if dp.SecretRefs == nil {
		dp.SecretRefs = []string{"*"}
	}
	if dp.Targets == nil {
		t := true
		dp.Targets = &t
	}
	if c.Artifacts.AllowUnlisted == nil {
		t := true
		c.Artifacts.AllowUnlisted = &t
	}
	c.Pipeline.applyDefaults()
}

// SecurityWarnings lists the settings fit only for development.
func (c *Config) SecurityWarnings() []string {
	var w []string
	if c.Auth.Mode == AuthModeNone {
		w = append(w, "auth.mode=none: API senza autenticazione e permessi")
	}
	dp := c.DescriptorPolicy
	for _, ps := range dp.Params {
		if slices.Contains(ps, "*") {
			w = append(w, "descriptor_policy.params: il descrittore può impostare qualsiasi parametro (anche comandi come post_command)")
			break
		}
	}
	if slices.Contains(dp.SecretRefs, "*") {
		w = append(w, "descriptor_policy.secret_refs: il descrittore può dichiarare qualsiasi segreto (anche variabili d'ambiente del worker)")
	}
	if c.Artifacts.AllowUnlisted != nil && *c.Artifacts.AllowUnlisted {
		w = append(w, "artifacts.allow_unlisted: il worker scarica artefatti da qualsiasi sorgente, anche file locali")
	}
	w = append(w, c.Pipeline.securityWarnings()...)
	return w
}

// Load reads the YAML file (if given) on top of the defaults and applies environment variables.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("configurazione non valida: %w", err)
		}
	}
	cfg.applyDefaults()
	if url := os.Getenv("DBOS_SYSTEM_DATABASE_URL"); url != "" {
		cfg.DBOS.DatabaseURL = url
	}
	if cfg.DBOS.ExecutorID == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("dbos.executor_id non impostato e hostname non disponibile: %w", err)
		}
		cfg.DBOS.ExecutorID = host
	}
	return cfg, cfg.Validate()
}

// Validate checks the consistency of the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.DBOS.DatabaseURL == "" {
		errs = append(errs, errors.New("dbos.database_url obbligatorio"))
	}
	if c.DBOS.Concurrency < 1 || c.DBOS.EnvironmentConcurrency < 1 || c.DBOS.GlobalConcurrency < 0 {
		errs = append(errs, errors.New("dbos: concurrency ed environment_concurrency devono essere >= 1, global_concurrency >= 0"))
	}
	switch c.EnvConfig.Provider {
	case EnvProviderNone, EnvProviderConsul, EnvProviderFile:
	default:
		errs = append(errs, fmt.Errorf("envconfig.provider %q non supportato (none|consul|file)", c.EnvConfig.Provider))
	}
	if c.Callbacks.Timeout <= 0 || c.Callbacks.MaxRetries < 0 {
		errs = append(errs, errors.New("callbacks: timeout deve essere > 0 e max_retries >= 0"))
	}
	errs = append(errs, c.validateSecurity()...)
	errs = append(errs, c.validateOperations()...)
	errs = append(errs, c.validateSIEM()...)
	errs = append(errs, c.validateIntegrations()...)
	errs = append(errs, c.Pipeline.validateCustom()...)
	if g := c.DBOS.GlobalConcurrency; g > 0 && c.DBOS.EnvironmentConcurrency > g {
		errs = append(errs, errors.New("dbos.environment_concurrency non può superare global_concurrency"))
	}
	seen := map[string]bool{}
	for _, s := range c.Pipeline.Steps {
		seen[s.Name] = false
	}
	for _, t := range slices.Sorted(maps.Keys(c.Rollback.ArtifactTypes)) {
		rt := c.Rollback.ArtifactTypes[t]
		if rt.Enabled && !rt.Allowed {
			errs = append(errs, fmt.Errorf("rollback.artifact_types.%s: enabled richiede allowed", t))
		}
		for _, s := range rt.On {
			if _, ok := seen[s]; !ok {
				errs = append(errs, fmt.Errorf("rollback.artifact_types.%s.on: step %q non presente nella pipeline", t, s))
			}
		}
	}
	clear(seen)
	for _, s := range c.Pipeline.Steps {
		if s.Mode != ModeMandatory && s.Mode != ModeOptional {
			errs = append(errs, fmt.Errorf("step %q: mode deve essere %q o %q", s.Name, ModeMandatory, ModeOptional))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("step %q duplicato", s.Name))
		}
		seen[s.Name] = true
	}
	return errors.Join(errs...)
}

// validateOperations checks drain, recovery, retention and observability.
// validateSIEM checks the structured log destinations.
func (c *Config) validateSIEM() []error {
	if !c.SIEM.Enabled {
		return nil
	}
	targets := c.SIEM.Targets()
	if len(targets) == 0 {
		return []error{errors.New("siem: con enabled serve address (udp, tcp) o una destinazione in destinations")}
	}
	var errs []error
	for i, d := range targets {
		switch d.Network {
		case "udp", "tcp":
			if d.Address == "" {
				errs = append(errs, fmt.Errorf("siem: destinazione %d (%s) senza address", i+1, d.Network))
			}
		case "unixgram", "journald":
		default:
			errs = append(errs, fmt.Errorf("siem: rete %q non supportata (udp, tcp, unixgram, journald)", d.Network))
		}
	}
	return errs
}

func (c *Config) validateOperations() []error {
	var errs []error
	if c.DBOS.DrainTimeout < 0 {
		errs = append(errs, errors.New("dbos.drain_timeout deve essere >= 0"))
	}
	r := c.Recovery
	if r.HeartbeatInterval <= 0 || r.StaleAfter < 2*r.HeartbeatInterval {
		errs = append(errs, errors.New("recovery: heartbeat_interval deve essere > 0 e stale_after almeno il doppio"))
	}
	if r.AutoRecoverAfter < 0 || (r.AutoRecoverAfter > 0 && r.AutoRecoverAfter < r.StaleAfter) {
		errs = append(errs, errors.New("recovery.auto_recover_after deve essere 0 (disattivata) o almeno stale_after"))
	}
	if t := c.Retention; t.MaxAge < 0 || t.KeepReleases < 1 || t.Interval <= 0 {
		errs = append(errs, errors.New("retention: max_age deve essere >= 0, keep_releases >= 1 e interval > 0"))
	}
	if t := c.Retention; t.MaxAge > 0 && t.MaxAge < 24*time.Hour {
		errs = append(errs, errors.New("retention.max_age deve essere almeno 24h"))
	}
	if c.DBOS.Region != "" && !regionRe.MatchString(c.DBOS.Region) {
		errs = append(errs, fmt.Errorf("dbos.region %q non valida", c.DBOS.Region))
	}
	if c.Chains.PollInterval < 100*time.Millisecond {
		errs = append(errs, errors.New("chains.poll_interval deve essere almeno 100ms"))
	}
	if c.Rollout.CheckTimeout <= 0 {
		errs = append(errs, errors.New("rollout.check_timeout deve essere > 0"))
	}
	if s := c.Observability.Tracing.SampleRatio; s < 0 || s > 1 {
		errs = append(errs, errors.New("observability.tracing.sample_ratio deve essere tra 0 e 1"))
	}
	return errs
}

// regionRe are the allowed region names (like environment names).
var regionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// tokenHashRe is a hexadecimal sha256.
var tokenHashRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// validateSecurity checks authentication, secrets and artifact sources.
func (c *Config) validateSecurity() []error {
	var errs []error
	switch c.Auth.Mode {
	case AuthModeNone:
	case AuthModeEnabled:
		if c.Auth.OIDC == nil && len(c.Auth.Tokens) == 0 && c.Auth.TokensFile == "" {
			errs = append(errs, errors.New("auth: con mode enabled configurare oidc, tokens o tokens_file"))
		}
		if o := c.Auth.OIDC; o != nil && (o.Issuer == "" || o.Audience == "") {
			errs = append(errs, errors.New("auth.oidc: issuer e audience obbligatori"))
		}
		if o := c.Auth.OIDC; o != nil && o.UI != nil {
			if o.UI.ClientID == "" {
				errs = append(errs, errors.New("auth.oidc.ui.client_id obbligatorio"))
			}
			if p := o.UI.RedirectPath; p != "" && (!strings.HasPrefix(p, "/") || strings.HasPrefix(p, "/api/")) {
				errs = append(errs, errors.New("auth.oidc.ui.redirect_path deve essere un percorso assoluto fuori da /api/"))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("auth.mode %q non valido (none|enabled)", c.Auth.Mode))
	}
	names := map[string]bool{}
	for i, t := range c.Auth.Tokens {
		if t.Name == "" || names[t.Name] {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: name obbligatorio e univoco", i))
		}
		names[t.Name] = true
		if !tokenHashRe.MatchString(t.SHA256) {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: sha256 deve essere l'hash esadecimale del token, mai il token", i))
		}
	}
	if v := c.Secrets.Vault; v.Enabled {
		switch v.Auth {
		case "", "token", "approle", "kubernetes":
		default:
			errs = append(errs, fmt.Errorf("secrets.vault.auth %q non valido (token|approle|kubernetes)", v.Auth))
		}
	}
	if c.Artifacts.Timeout <= 0 {
		errs = append(errs, errors.New("artifacts.timeout deve essere > 0"))
	}
	if c.Audit.MaxSizeMB < 0 {
		errs = append(errs, errors.New("audit.max_size_mb deve essere >= 0"))
	}
	if c.Audit.HMACKeyRef != "" && c.Audit.File == "" {
		errs = append(errs, errors.New("audit.hmac_key_ref richiede audit.file"))
	}
	return errs
}
