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
	Reconcile        Reconcile                 `yaml:"reconcile"`
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
	// Other metrics systems for the gates (rollout.health.metrics[].provider).
	Datadog      *DatadogMetrics      `yaml:"datadog"`
	NewRelic     *NewRelicMetrics     `yaml:"newrelic"`
	Elastic      *ElasticMetrics      `yaml:"elastic"`
	CloudWatch   *CloudWatchMetrics   `yaml:"cloudwatch"`
	AzureMonitor *AzureMonitorMetrics `yaml:"azure_monitor"`
}

// DatadogMetrics configures the Datadog API: keys are secret references, read at every call.
type DatadogMetrics struct {
	// URL is the API site (default https://api.datadoghq.com; e.g. https://api.datadoghq.eu).
	URL       string `yaml:"url"`
	APIKeyRef string `yaml:"api_key_ref"`
	AppKeyRef string `yaml:"app_key_ref"`
}

// NewRelicMetrics configures NerdGraph with a user API key.
type NewRelicMetrics struct {
	// URL is the NerdGraph endpoint (default https://api.newrelic.com/graphql).
	URL       string `yaml:"url"`
	AccountID int64  `yaml:"account_id"`
	APIKeyRef string `yaml:"api_key_ref"`
}

// ElasticMetrics configures Elasticsearch (ES|QL): API key or user and password.
type ElasticMetrics struct {
	URL         string `yaml:"url"`
	APIKeyRef   string `yaml:"api_key_ref"`
	Username    string `yaml:"username"`
	PasswordRef string `yaml:"password_ref"`
	// CAFile is a bundle of extra CAs to reach Elasticsearch.
	CAFile string `yaml:"ca_file"`
}

// CloudWatchMetrics configures Amazon CloudWatch (GetMetricData). Credentials come from the
// SDK default chain or from the secret references, with an optional role to assume.
type CloudWatchMetrics struct {
	Region             string `yaml:"region"`
	Endpoint           string `yaml:"endpoint"`
	AccessKeyIDRef     string `yaml:"access_key_id_ref"`
	SecretAccessKeyRef string `yaml:"secret_access_key_ref"`
	RoleARN            string `yaml:"role_arn"`
	ExternalID         string `yaml:"external_id"`
}

// AzureMonitorMetrics configures Azure Monitor Logs (KQL on a Log Analytics workspace): a
// service principal with the secret reference of its client secret or, without it, the
// identity of the worker (workload or managed identity).
type AzureMonitorMetrics struct {
	WorkspaceID     string `yaml:"workspace_id"`
	Endpoint        string `yaml:"endpoint"`
	Authority       string `yaml:"authority"`
	TenantID        string `yaml:"tenant_id"`
	ClientID        string `yaml:"client_id"`
	ClientSecretRef string `yaml:"client_secret_ref"`
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

// Reconcile configures the periodic reading of the release in service on the targets, to
// report in the inventory the targets changed outside manent.
type Reconcile struct {
	// Interval between two readings of every environment (0 = disabled).
	Interval time.Duration `yaml:"interval"`
	// Timeout of the reading of one target.
	Timeout time.Duration `yaml:"timeout"`
	// Concurrency is the number of targets of an environment read together.
	Concurrency int `yaml:"concurrency"`
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
	// TenantsCache is how long the tenant listing is kept (selectors, permissions).
	TenantsCache time.Duration `yaml:"tenants_cache"`
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
	// TenantsKey is the key template of the tenant documents, with {tenant}.
	TenantsKey string `yaml:"tenants_key"`
}

// FileConfig configures reading from local files.
type FileConfig struct {
	// Path is the path template, with {application} and {environment}.
	Path string `yaml:"path"`
	// Tenants is the path template of the tenant documents, with {tenant}.
	Tenants string `yaml:"tenants"`
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
	// Tenants limits the binding to the deploys of the matching tenants (selector on tags and "name").
	Tenants string `yaml:"tenants"`
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
	// Engine is builtin (default: rules and freezes of this file) or opa (Rego policies).
	Engine string `yaml:"engine"`
	// Rules apply per application and environment: the first match wins. With the opa engine
	// they are the data of the default policy (data.manent.rules).
	Rules   []GovernanceRule `yaml:"rules"`
	Freezes []FreezeWindow   `yaml:"freezes"`
	// Refresh is how often a process reads the policy and the runtime freezes again;
	// MaxStaleness, how long without a successful read before real deploys are refused.
	Refresh      time.Duration `yaml:"refresh"`
	MaxStaleness time.Duration `yaml:"max_staleness"`
	// Start is what the deploy start checks again: deny (denials and freezes) or strict (also
	// obligations grown after the submission).
	Start string    `yaml:"start"`
	OPA   OPAConfig `yaml:"opa"`
}

// Governance engines, sources and start checks.
const (
	GovernanceBuiltin  = "builtin"
	GovernanceOPA      = "opa"
	PolicySourceStore  = "store"
	PolicySourceFile   = "file"
	PolicySourceBundle = "bundle"
	PolicySourceConsul = "consul"
	PolicyStartDeny    = "deny"
	PolicyStartStrict  = "strict"
)

// OPAConfig configures the Rego policies evaluated by OPA embedded in manent.
type OPAConfig struct {
	// Query is the decision (default data.manent.governance.decision).
	Query string `yaml:"query"`
	// Source of the revisions: store (default: managed with manent policy), file, bundle, consul.
	Source      string             `yaml:"source"`
	EvalTimeout time.Duration      `yaml:"eval_timeout"`
	Store       PolicyStoreConfig  `yaml:"store"`
	File        PolicyFileConfig   `yaml:"file"`
	Bundle      PolicyBundleConfig `yaml:"bundle"`
	Consul      PolicyConsulConfig `yaml:"consul"`
}

// PolicyStoreConfig configures the revisions managed by manent.
type PolicyStoreConfig struct {
	// Review requires approving a revision with an identity other than its author.
	Review bool `yaml:"review"`
	// Tests: run (default: the revision's tests run if present) or required.
	Tests string `yaml:"tests"`
}

// PolicyFileConfig reads the policy from a directory (*.rego and data.json/data.yaml).
type PolicyFileConfig struct {
	Dir string `yaml:"dir"`
}

// PolicyBundleConfig downloads a signed OPA bundle.
type PolicyBundleConfig struct {
	URL    string `yaml:"url"`
	CAFile string `yaml:"ca_file"`
	// CredentialsRef is the secret with the bearer token of the download.
	CredentialsRef string        `yaml:"credentials_ref"`
	Signing        BundleSigning `yaml:"signing"`
	// InsecureSkipVerify accepts unsigned bundles: development only.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// BundleSigning is the verification of the bundle signature.
type BundleSigning struct {
	// PublicKeyRef is the secret (or file:) with the public key in PEM (or the HMAC secret).
	PublicKeyRef string `yaml:"public_key_ref"`
	// Algorithm default RS256; KeyID and Scope as in the OPA bundle signature.
	Algorithm string `yaml:"algorithm"`
	KeyID     string `yaml:"key_id"`
	Scope     string `yaml:"scope"`
}

// PolicyConsulConfig reads the policy from the Consul KV store, with the address and the
// token of envconfig.consul.
type PolicyConsulConfig struct {
	Prefix string `yaml:"prefix"`
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
	// Tenants limits the rule to the deploys of the matching tenants (selector).
	Tenants string `yaml:"tenants"`
}

// ApprovalSettings configures the approvals required before the deploy.
type ApprovalSettings struct {
	Required  int           `yaml:"required"`
	AllowSelf bool          `yaml:"allow_self"`
	Timeout   time.Duration `yaml:"timeout"`
	// When: before (default) or rollout, to ask for them at the rollout's approval points.
	When string `yaml:"when"`
	// Approvers limits who can approve: OIDC or token groups, identities "<method>:<name>".
	Approvers *ApproversSettings `yaml:"approvers"`
}

// ApproversSettings are the identities allowed to approve ("*" patterns).
type ApproversSettings struct {
	Groups   []string `yaml:"groups"`
	Subjects []string `yaml:"subjects"`
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
	// Hard windows cannot be exempted by the policy nor overridden with freezeOverride.
	Hard bool `yaml:"hard"`
	// Tenants limits the window to the deploys of the matching tenants (selector).
	Tenants string `yaml:"tenants"`
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
			Provider:     EnvProviderNone,
			MaxRetries:   3,
			Consul:       ConsulConfig{Key: "manent/{application}/{environment}", TenantsKey: "manent/tenants/{tenant}"},
			File:         FileConfig{Path: "./envconfig/{application}/{environment}.yml", Tenants: "./envconfig/tenants/{tenant}.yml"},
			TenantsCache: 30 * time.Second,
		},
		Callbacks: Callbacks{Timeout: 10 * time.Second, MaxRetries: 3},
		// Rollback is allowed but disabled for applications; never for migrations, which usually
		// cannot be reversed.
		Rollback: Rollback{Locator: "plugin", ArtifactTypes: map[string]RollbackType{
			"application":   {Allowed: true},
			"container":     {Allowed: true},
			"kubernetes":    {Allowed: true},
			"ecs":           {Allowed: true},
			"lambda":        {Allowed: true},
			"cloudrun":      {Allowed: true},
			"war":           {Allowed: true},
			"ear":           {Allowed: true},
			"windows":       {Allowed: true},
			"appservice":    {Allowed: true},
			"containerapps": {Allowed: true},
			"migration":     {Allowed: false},
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
		Reconcile:     Reconcile{Timeout: 30 * time.Second, Concurrency: 8},
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
	if g := c.Governance; g.Engine == GovernanceOPA && g.OPA.Source == PolicySourceBundle && g.OPA.Bundle.InsecureSkipVerify {
		w = append(w, "governance.opa.bundle.insecure_skip_verify: bundle della policy non firmati accettati")
	}
	if g := c.Governance; g.Engine == GovernanceOPA && g.OPA.Source == PolicySourceStore && !g.OPA.Store.Review {
		w = append(w, "governance.opa.store.review: false, chi carica una revisione della policy può attivarla da solo")
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
			return nil, fmt.Errorf("invalid configuration: %w", err)
		}
	}
	cfg.applyDefaults()
	if url := os.Getenv("DBOS_SYSTEM_DATABASE_URL"); url != "" {
		cfg.DBOS.DatabaseURL = url
	}
	if cfg.DBOS.ExecutorID == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("dbos.executor_id not set and hostname not available: %w", err)
		}
		cfg.DBOS.ExecutorID = host
	}
	return cfg, cfg.Validate()
}

// Validate checks the consistency of the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.DBOS.DatabaseURL == "" {
		errs = append(errs, errors.New("dbos.database_url is required"))
	}
	if c.DBOS.Concurrency < 1 || c.DBOS.EnvironmentConcurrency < 1 || c.DBOS.GlobalConcurrency < 0 {
		errs = append(errs, errors.New("dbos: concurrency and environment_concurrency must be >= 1, global_concurrency >= 0"))
	}
	switch c.EnvConfig.Provider {
	case EnvProviderNone, EnvProviderConsul, EnvProviderFile:
	default:
		errs = append(errs, fmt.Errorf("envconfig.provider %q not supported (none|consul|file)", c.EnvConfig.Provider))
	}
	if c.Callbacks.Timeout <= 0 || c.Callbacks.MaxRetries < 0 {
		errs = append(errs, errors.New("callbacks: timeout must be > 0 and max_retries >= 0"))
	}
	errs = append(errs, c.validateSecurity()...)
	errs = append(errs, c.validateOperations()...)
	errs = append(errs, c.validateSIEM()...)
	errs = append(errs, c.validateIntegrations()...)
	errs = append(errs, c.validateMetricSources()...)
	errs = append(errs, c.validateGovernance()...)
	errs = append(errs, c.Pipeline.validateCustom()...)
	if g := c.DBOS.GlobalConcurrency; g > 0 && c.DBOS.EnvironmentConcurrency > g {
		errs = append(errs, errors.New("dbos.environment_concurrency cannot exceed global_concurrency"))
	}
	seen := map[string]bool{}
	for _, s := range c.Pipeline.Steps {
		seen[s.Name] = false
	}
	for _, t := range slices.Sorted(maps.Keys(c.Rollback.ArtifactTypes)) {
		rt := c.Rollback.ArtifactTypes[t]
		if rt.Enabled && !rt.Allowed {
			errs = append(errs, fmt.Errorf("rollback.artifact_types.%s: enabled requires allowed", t))
		}
		for _, s := range rt.On {
			if _, ok := seen[s]; !ok {
				errs = append(errs, fmt.Errorf("rollback.artifact_types.%s.on: step %q not in the pipeline", t, s))
			}
		}
	}
	clear(seen)
	for _, s := range c.Pipeline.Steps {
		if s.Mode != ModeMandatory && s.Mode != ModeOptional {
			errs = append(errs, fmt.Errorf("step %q: mode must be %q or %q", s.Name, ModeMandatory, ModeOptional))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("duplicate step %q", s.Name))
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
		return []error{errors.New("siem: with enabled, address (udp, tcp) or an entry in destinations is required")}
	}
	var errs []error
	for i, d := range targets {
		switch d.Network {
		case "udp", "tcp":
			if d.Address == "" {
				errs = append(errs, fmt.Errorf("siem: destination %d (%s) without address", i+1, d.Network))
			}
		case "unixgram", "journald":
		default:
			errs = append(errs, fmt.Errorf("siem: network %q not supported (udp, tcp, unixgram, journald)", d.Network))
		}
	}
	return errs
}

// validateMetricSources checks the metrics systems of the gates.
func (c *Config) validateMetricSources() []error {
	var errs []error
	r := c.Rollout
	if d := r.Datadog; d != nil && (d.APIKeyRef == "" || d.AppKeyRef == "" || (d.URL != "" && !validURL(d.URL))) {
		errs = append(errs, errors.New("rollout.datadog: api_key_ref and app_key_ref are required, http(s) url if set"))
	}
	if n := r.NewRelic; n != nil && (n.AccountID <= 0 || n.APIKeyRef == "" || (n.URL != "" && !validURL(n.URL))) {
		errs = append(errs, errors.New("rollout.newrelic: account_id and api_key_ref are required, http(s) url if set"))
	}
	if e := r.Elastic; e != nil && (!validURL(e.URL) || (e.APIKeyRef == "") == (e.Username == "") || (e.Username != "" && e.PasswordRef == "")) {
		errs = append(errs, errors.New("rollout.elastic: url is required, plus api_key_ref or username with password_ref"))
	}
	if w := r.CloudWatch; w != nil && (w.Region == "" || (w.AccessKeyIDRef == "") != (w.SecretAccessKeyRef == "") || (w.Endpoint != "" && !validURL(w.Endpoint))) {
		errs = append(errs, errors.New("rollout.cloudwatch: region is required, access_key_id_ref and secret_access_key_ref together, http(s) endpoint if set"))
	}
	if a := r.AzureMonitor; a != nil && (!guidRe.MatchString(a.WorkspaceID) || (a.ClientSecretRef != "" && (!guidRe.MatchString(a.TenantID) || !guidRe.MatchString(a.ClientID))) ||
		(a.Endpoint != "" && !validURL(a.Endpoint)) || (a.Authority != "" && !validURL(a.Authority))) {
		errs = append(errs, errors.New("rollout.azure_monitor: workspace_id is required (GUID), client_secret_ref with tenant_id and client_id, http(s) endpoint and authority if set"))
	}
	return errs
}

func (c *Config) validateOperations() []error {
	var errs []error
	if c.DBOS.DrainTimeout < 0 {
		errs = append(errs, errors.New("dbos.drain_timeout must be >= 0"))
	}
	r := c.Recovery
	if r.HeartbeatInterval <= 0 || r.StaleAfter < 2*r.HeartbeatInterval {
		errs = append(errs, errors.New("recovery: heartbeat_interval must be > 0 and stale_after at least twice as long"))
	}
	if r.AutoRecoverAfter < 0 || (r.AutoRecoverAfter > 0 && r.AutoRecoverAfter < r.StaleAfter) {
		errs = append(errs, errors.New("recovery.auto_recover_after must be 0 (disabled) or at least stale_after"))
	}
	if t := c.Retention; t.MaxAge < 0 || t.KeepReleases < 1 || t.Interval <= 0 {
		errs = append(errs, errors.New("retention: max_age must be >= 0, keep_releases >= 1 and interval > 0"))
	}
	if t := c.Retention; t.MaxAge > 0 && t.MaxAge < 24*time.Hour {
		errs = append(errs, errors.New("retention.max_age must be at least 24h"))
	}
	if t := c.Reconcile; t.Interval < 0 || (t.Interval > 0 && t.Interval < 10*time.Second) || t.Timeout <= 0 || t.Concurrency < 1 {
		errs = append(errs, errors.New("reconcile: interval must be 0 (disabled) or at least 10s, timeout > 0 and concurrency >= 1"))
	}
	if c.DBOS.Region != "" && !regionRe.MatchString(c.DBOS.Region) {
		errs = append(errs, fmt.Errorf("invalid dbos.region %q", c.DBOS.Region))
	}
	if c.Chains.PollInterval < 100*time.Millisecond {
		errs = append(errs, errors.New("chains.poll_interval must be at least 100ms"))
	}
	if c.Rollout.CheckTimeout <= 0 {
		errs = append(errs, errors.New("rollout.check_timeout must be > 0"))
	}
	if s := c.Observability.Tracing.SampleRatio; s < 0 || s > 1 {
		errs = append(errs, errors.New("observability.tracing.sample_ratio must be between 0 and 1"))
	}
	return errs
}

// regionRe are the allowed region names (like environment names).
var regionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// tokenHashRe is a hexadecimal sha256.
var tokenHashRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// guidRe is an Azure ID (workspace, tenant, client).
var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateSecurity checks authentication, secrets and artifact sources.
func (c *Config) validateSecurity() []error {
	var errs []error
	switch c.Auth.Mode {
	case AuthModeNone:
	case AuthModeEnabled:
		if c.Auth.OIDC == nil && len(c.Auth.Tokens) == 0 && c.Auth.TokensFile == "" {
			errs = append(errs, errors.New("auth: with mode enabled configure oidc, tokens or tokens_file"))
		}
		if o := c.Auth.OIDC; o != nil && (o.Issuer == "" || o.Audience == "") {
			errs = append(errs, errors.New("auth.oidc: issuer and audience are required"))
		}
		if o := c.Auth.OIDC; o != nil && o.UI != nil {
			if o.UI.ClientID == "" {
				errs = append(errs, errors.New("auth.oidc.ui.client_id is required"))
			}
			if p := o.UI.RedirectPath; p != "" && (!strings.HasPrefix(p, "/") || strings.HasPrefix(p, "/api/")) {
				errs = append(errs, errors.New("auth.oidc.ui.redirect_path must be an absolute path outside /api/"))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("auth.mode %q is invalid (none|enabled)", c.Auth.Mode))
	}
	names := map[string]bool{}
	for i, t := range c.Auth.Tokens {
		if t.Name == "" || names[t.Name] {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: name is required and must be unique", i))
		}
		names[t.Name] = true
		if !tokenHashRe.MatchString(t.SHA256) {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: sha256 must be the hex hash of the token, never the token", i))
		}
	}
	if v := c.Secrets.Vault; v.Enabled {
		switch v.Auth {
		case "", "token", "approle", "kubernetes":
		default:
			errs = append(errs, fmt.Errorf("secrets.vault.auth %q is invalid (token|approle|kubernetes)", v.Auth))
		}
	}
	if c.Artifacts.Timeout <= 0 {
		errs = append(errs, errors.New("artifacts.timeout must be > 0"))
	}
	if c.Audit.MaxSizeMB < 0 {
		errs = append(errs, errors.New("audit.max_size_mb must be >= 0"))
	}
	if c.Audit.HMACKeyRef != "" && c.Audit.File == "" {
		errs = append(errs, errors.New("audit.hmac_key_ref requires audit.file"))
	}
	return errs
}

// validateGovernance checks engine, source and start checks of the governance policy.
func (c *Config) validateGovernance() []error {
	g := c.Governance
	var errs []error
	switch g.Engine {
	case "", GovernanceBuiltin, GovernanceOPA:
	default:
		errs = append(errs, fmt.Errorf("governance.engine %q not supported (builtin|opa)", g.Engine))
	}
	switch g.Start {
	case "", PolicyStartDeny, PolicyStartStrict:
	default:
		errs = append(errs, fmt.Errorf("governance.start %q not supported (deny|strict)", g.Start))
	}
	if g.Refresh < 0 || g.MaxStaleness < 0 {
		errs = append(errs, errors.New("governance: refresh and max_staleness must be >= 0"))
	}
	if g.MaxStaleness > 0 && g.Refresh > 0 && g.MaxStaleness <= g.Refresh {
		errs = append(errs, errors.New("governance.max_staleness must be greater than refresh"))
	}
	for i, r := range g.Rules {
		if a := r.Approvals; a != nil && a.Approvers != nil && len(a.Approvers.Groups) == 0 && len(a.Approvers.Subjects) == 0 {
			errs = append(errs, fmt.Errorf("governance.rules[%d].approvals.approvers: provide groups or subjects", i))
		}
	}
	if g.Engine != GovernanceOPA {
		return errs
	}
	o := g.OPA
	switch o.Source {
	case "", PolicySourceStore:
		switch o.Store.Tests {
		case "", "run", "required":
		default:
			errs = append(errs, fmt.Errorf("governance.opa.store.tests %q not supported (run|required)", o.Store.Tests))
		}
	case PolicySourceFile:
		if o.File.Dir == "" {
			errs = append(errs, errors.New("governance.opa.file.dir is required with source file"))
		}
	case PolicySourceBundle:
		if o.Bundle.URL == "" {
			errs = append(errs, errors.New("governance.opa.bundle.url is required with source bundle"))
		}
		if o.Bundle.Signing.PublicKeyRef == "" && !o.Bundle.InsecureSkipVerify {
			errs = append(errs, errors.New("governance.opa.bundle.signing.public_key_ref is required (or insecure_skip_verify: true, development only)"))
		}
	case PolicySourceConsul:
	default:
		errs = append(errs, fmt.Errorf("governance.opa.source %q not supported (store|file|bundle|consul)", o.Source))
	}
	if o.EvalTimeout < 0 {
		errs = append(errs, errors.New("governance.opa.eval_timeout must be >= 0"))
	}
	return errs
}
