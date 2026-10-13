// Package config gestisce la configurazione applicativa statica di manent.
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
)

// Modalità di uno step nella pipeline.
const (
	ModeMandatory = "mandatory"
	ModeOptional  = "optional"
)

// Config è la configurazione completa dell'applicazione.
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
	// DescriptorPolicy limita ciò che i descrittori possono aggiungere alla configurazione di ambiente.
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
}

// Observability configura metriche, tracing e sonde dei processi di lunga durata.
type Observability struct {
	// Listen, se indicato, avvia su questo indirizzo un server con /metrics, /healthz e
	// /readyz: serve al worker, che non ha l'API. Con serve le stesse rotte sono anche
	// sull'indirizzo dell'API.
	Listen string `yaml:"listen"`
	// Metrics abilita le metriche Prometheus.
	Metrics bool    `yaml:"metrics"`
	Tracing Tracing `yaml:"tracing"`
}

// Tracing configura l'esportazione delle tracce OpenTelemetry via OTLP/HTTP.
type Tracing struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint del collector (es. otel-collector:4318); vuoto: OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint"`
	// Insecure usa http invece di https.
	Insecure bool `yaml:"insecure"`
	// SampleRatio è la frazione delle tracce campionate (default 1).
	SampleRatio float64 `yaml:"sample_ratio"`
}

// RolloutConfig configura l'health gate dei rollout a ondate.
type RolloutConfig struct {
	// PrometheusURL è l'API di Prometheus per le metriche dei gate (rollout.health.metrics).
	PrometheusURL string `yaml:"prometheus_url"`
	// AlertmanagerURL è l'API di Alertmanager per gli allarmi (rollout.health.alerts).
	AlertmanagerURL string `yaml:"alertmanager_url"`
	// CheckTimeout limita ogni lettura dell'health gate.
	CheckTimeout time.Duration `yaml:"check_timeout"`
}

// Chains configura le dipendenze tra deploy (after) e le release composte.
type Chains struct {
	// PollInterval è il primo intervallo di lettura dello stato dei deploy attesi; raddoppia a
	// ogni lettura fino a 1 minuto.
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Recovery configura il registro dei worker e la ripresa dei workflow di un worker perso.
type Recovery struct {
	// HeartbeatInterval è ogni quanto un worker aggiorna il registro.
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	// StaleAfter è il ritardo di heartbeat oltre il quale un worker è considerato perso.
	StaleAfter time.Duration `yaml:"stale_after"`
	// AutoRecoverAfter, se > 0, fa riprendere a un altro worker i workflow in sospeso di un
	// worker della stessa versione arrestato o senza heartbeat da questo tempo. 0 = solo
	// manuale (manent admin recover).
	AutoRecoverAfter time.Duration `yaml:"auto_recover_after"`
}

// Retention configura la pulizia dello storico dei workflow.
type Retention struct {
	// MaxAge: i workflow conclusi da più di MaxAge vengono eliminati (0 = mai).
	MaxAge time.Duration `yaml:"max_age"`
	// KeepReleases sono i deploy riusciti conservati comunque per applicazione e ambiente.
	KeepReleases int `yaml:"keep_releases"`
	// Interval è ogni quanto un worker esegue la pulizia.
	Interval time.Duration `yaml:"interval"`
}

// App identifica l'applicazione.
type App struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// DBOS configura il motore di workflow e la coda dei deploy.
type DBOS struct {
	DatabaseURL string `yaml:"database_url"`
	// ExecutorID identifica il worker: deve essere univoco tra i nodi e stabile tra i riavvii,
	// perché all'avvio un worker recupera solo i workflow rimasti in sospeso con il proprio ID.
	// Default: hostname. La variabile DBOS__VMID ha la precedenza.
	ExecutorID string `yaml:"executor_id"`
	Queue      string `yaml:"queue"`
	// Concurrency è il numero massimo di deploy in esecuzione su ogni worker.
	Concurrency int `yaml:"concurrency"`
	// GlobalConcurrency è il numero massimo di deploy in esecuzione su tutti i worker (0 = nessun limite).
	GlobalConcurrency int `yaml:"global_concurrency"`
	// EnvironmentConcurrency è il numero massimo di deploy contemporanei della stessa
	// application/environment su tutti i worker.
	EnvironmentConcurrency int `yaml:"environment_concurrency"`
	// SchedulePollInterval è ogni quanto le modifiche alle schedule vengono rilette dal database.
	SchedulePollInterval time.Duration `yaml:"schedule_poll_interval"`
	// Region è la region del worker: con RegionRouting esegue i passi dei rollout a ondate
	// sui target di questa region (coda <queue>.<region>).
	Region string `yaml:"region"`
	// RegionRouting fa eseguire i passi dei rollout ai worker della region dei target, come
	// workflow figli; senza, l'orchestratore li esegue in linea.
	RegionRouting bool `yaml:"region_routing"`
	// DrainTimeout è il tempo massimo per cui un worker in arresto attende i deploy in corso,
	// dopo aver smesso di prelevarne dalla coda (0 = non attende).
	DrainTimeout time.Duration `yaml:"drain_timeout"`
}

// HTTP configura il web server API.
type HTTP struct {
	Listen string `yaml:"listen"`
	UI     UI     `yaml:"ui"`
}

// UI configura l'interfaccia web servita dall'API.
type UI struct {
	// Enabled serve l'interfaccia su / (default true).
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled indica se l'interfaccia web è attiva (default true).
func (u UI) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// Logging configura il log applicativo.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// SIEM configura l'invio dei log strutturati: a un collettore RFC 5424 (udp, tcp), al socket
// syslog locale (unixgram, es. /dev/log) o a journald con il protocollo nativo (journald).
type SIEM struct {
	Enabled bool   `yaml:"enabled"`
	Network string `yaml:"network"`
	Address string `yaml:"address"`
	// Destinations sono destinazioni in aggiunta a network/address (es. il SIEM remoto e il
	// journal locale insieme).
	Destinations []SIEMDestination `yaml:"destinations"`
}

// SIEMDestination è una destinazione dei log strutturati. Per unixgram e journald address è il
// percorso del socket (default /dev/log e /run/systemd/journal/socket).
type SIEMDestination struct {
	Network string `yaml:"network"`
	Address string `yaml:"address"`
}

// Targets restituisce le destinazioni attive: nessuna se l'invio è disattivato.
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

// Provider di configurazione di ambiente supportati.
const (
	EnvProviderNone   = "none"
	EnvProviderConsul = "consul"
	EnvProviderFile   = "file"
)

// EnvConfig configura il sistema da cui leggere la configurazione di deploy di ogni ambiente.
type EnvConfig struct {
	// Provider è none, consul o file.
	Provider string `yaml:"provider"`
	// Required fa fallire il deploy se per l'ambiente non esiste configurazione.
	Required bool `yaml:"required"`
	// MaxRetries sono i tentativi aggiuntivi in caso di errore di lettura.
	MaxRetries int          `yaml:"max_retries"`
	Consul     ConsulConfig `yaml:"consul"`
	File       FileConfig   `yaml:"file"`
}

// ConsulConfig configura la lettura dal KV store di Consul.
// Il token ACL non va scritto nel file: si legge da CONSUL_HTTP_TOKEN o da token_file.
type ConsulConfig struct {
	// Address dell'agent (default CONSUL_HTTP_ADDR o http://127.0.0.1:8500).
	Address    string `yaml:"address"`
	Datacenter string `yaml:"datacenter"`
	// Key è il modello di chiave, con {application} e {environment}.
	Key       string `yaml:"key"`
	TokenFile string `yaml:"token_file"`
}

// FileConfig configura la lettura da file locali.
type FileConfig struct {
	// Path è il modello di percorso, con {application} e {environment}.
	Path string `yaml:"path"`
}

// Callbacks configura l'invio degli eventi ai callback indicati nei descrittori.
type Callbacks struct {
	// Timeout di ogni chiamata.
	Timeout time.Duration `yaml:"timeout"`
	// MaxRetries sono i tentativi aggiuntivi di ogni invio.
	MaxRetries int `yaml:"max_retries"`
	// AllowHTTP ammette callback http; di default solo https.
	AllowHTTP bool `yaml:"allow_http"`
	// AllowedHosts limita gli host dei callback (anche "*.dominio"); vuoto = tutti.
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// Rollback configura le policy di rollback automatico.
type Rollback struct {
	// Locator è la strategia di default per individuare la release a cui tornare.
	Locator string `yaml:"locator"`
	// ArtifactTypes è la policy per tipo di artefatto. Per i tipi non elencati il rollback
	// non è ammesso.
	ArtifactTypes map[string]RollbackType `yaml:"artifact_types"`
}

// RollbackType è la policy di rollback di un tipo di artefatto.
type RollbackType struct {
	// Allowed indica se il rollback è possibile per questo tipo (es. no per le migration di database).
	Allowed bool `yaml:"allowed"`
	// Enabled è il default, sovrascrivibile da descrittore e configurazione di ambiente.
	Enabled bool `yaml:"enabled"`
	// On elenca gli step il cui fallimento avvia il rollback (default: DefaultRollbackOn).
	On []string `yaml:"on"`
	// Locator sovrascrive il locator di default per questo tipo.
	Locator string `yaml:"locator"`
}

// DefaultRollbackOn sono gli step che di default avviano il rollback: quelli successivi
// alla prima modifica degli ambienti.
var DefaultRollbackOn = []string{"deploy", "post-test", "activation"}

// Modalità di autenticazione.
const (
	// AuthModeNone disattiva autenticazione e permessi: solo per sviluppo.
	AuthModeNone    = "none"
	AuthModeEnabled = "enabled"
)

// Auth configura l'autenticazione dell'API.
type Auth struct {
	// Mode è none (default) o enabled.
	Mode   string     `yaml:"mode"`
	OIDC   *OIDC      `yaml:"oidc"`
	Tokens []APIToken `yaml:"tokens"`
	// TokensFile è un file YAML con altri token (chiave tokens), riletto quando cambia:
	// permette di aggiungere e revocare token senza riavviare.
	TokensFile string `yaml:"tokens_file"`
}

// OIDC configura la verifica dei token di un identity provider OIDC.
type OIDC struct {
	Issuer        string `yaml:"issuer"`
	Audience      string `yaml:"audience"`
	UsernameClaim string `yaml:"username_claim"`
	GroupsClaim   string `yaml:"groups_claim"`
	CAFile        string `yaml:"ca_file"`
	// UI è il client OIDC pubblico (Authorization Code + PKCE) dell'interfaccia web: i suoi
	// access token devono avere l'audience e il claim dei gruppi verificati qui sopra.
	UI *OIDCUI `yaml:"ui"`
}

// OIDCUI è il client OIDC dell'interfaccia web.
type OIDCUI struct {
	ClientID string   `yaml:"client_id"`
	Scopes   []string `yaml:"scopes"`
	// RedirectPath è il percorso di ritorno dopo il login (default /auth/callback).
	RedirectPath string `yaml:"redirect_path"`
}

// APIToken è un token statico per sistemi automatici: in configurazione c'è solo lo sha256.
type APIToken struct {
	Name   string   `yaml:"name"`
	SHA256 string   `yaml:"sha256"`
	Groups []string `yaml:"groups"`
	// ExpiresAt, se indicato, è la scadenza del token.
	ExpiresAt time.Time `yaml:"expires_at"`
}

// RBAC assegna i ruoli per applicazione e ambiente.
type RBAC struct {
	// Roles definisce ruoli aggiuntivi a quelli predefiniti (viewer, tester, deployer, operator, admin).
	Roles    map[string][]string `yaml:"roles"`
	Bindings []RBACBinding       `yaml:"bindings"`
}

// RBACBinding assegna un ruolo a utenti o gruppi su applicazioni e ambienti (pattern con "*").
type RBACBinding struct {
	Subjects     []string `yaml:"subjects"`
	Groups       []string `yaml:"groups"`
	Role         string   `yaml:"role"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
}

// Audit configura la registrazione delle azioni.
type Audit struct {
	// File, se indicato, riceve le voci di audit in JSON Lines (oltre al log e al SIEM),
	// concatenate da un hash che rende rilevabili modifiche e cancellazioni.
	File string `yaml:"file"`
	// MaxSizeMB è la dimensione oltre cui il file viene ruotato (0 = nessuna rotazione).
	MaxSizeMB int `yaml:"max_size_mb"`
	// HMACKeyRef è il riferimento a una chiave segreta: con la chiave la catena è un HMAC e
	// non può essere ricalcolata da chi modifica il file.
	HMACKeyRef string `yaml:"hmac_key_ref"`
}

// Governance configura le regole di rilascio: promozione, approvazioni e freeze.
type Governance struct {
	// Rules si applicano per applicazione e ambiente: vale la prima che corrisponde.
	Rules   []GovernanceRule `yaml:"rules"`
	Freezes []FreezeWindow   `yaml:"freezes"`
}

// GovernanceRule sono le regole di rilascio di applicazioni e ambienti (pattern con "*").
type GovernanceRule struct {
	Name         string   `yaml:"name"`
	Applications []string `yaml:"applications"`
	Environments []string `yaml:"environments"`
	// Requires elenca gli ambienti in cui lo stesso artefatto deve essere già stato rilasciato.
	Requires  []string          `yaml:"requires"`
	Approvals *ApprovalSettings `yaml:"approvals"`
}

// ApprovalSettings configura le approvazioni richieste prima del deploy.
type ApprovalSettings struct {
	Required  int           `yaml:"required"`
	AllowSelf bool          `yaml:"allow_self"`
	Timeout   time.Duration `yaml:"timeout"`
}

// FreezeWindow è una finestra in cui i deploy sono bloccati: assoluta (from/to) o
// ricorrente (cron a 5 campi per duration).
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

// DescriptorPolicy limita ciò che un descrittore può aggiungere alla configurazione di ambiente.
// I campi assenti valgono "nessun limite", per compatibilità: in produzione vanno impostati.
type DescriptorPolicy struct {
	// Params elenca per plugin ("*" = tutti) i pattern dei parametri impostabili dal descrittore.
	Params map[string][]string `yaml:"params"`
	// SecretRefs sono i pattern dei riferimenti ai segreti dichiarabili dal descrittore,
	// con {application} e {environment}.
	SecretRefs []string `yaml:"secret_refs"`
	// Targets ammette target indicati dal descrittore.
	Targets *bool `yaml:"targets"`
}

// Secrets configura i provider di segreti aggiuntivi a env: e file:.
type Secrets struct {
	Vault Vault `yaml:"vault"`
}

// Vault configura lo schema "vault:".
type Vault struct {
	Enabled bool `yaml:"enabled"`
	// Address default VAULT_ADDR.
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
	// Auth è token (default), approle o kubernetes.
	Auth       string        `yaml:"auth"`
	TokenFile  string        `yaml:"token_file"`
	AppRole    VaultAppRole  `yaml:"approle"`
	Kubernetes VaultK8s      `yaml:"kubernetes"`
	CAFile     string        `yaml:"ca_file"`
	Timeout    time.Duration `yaml:"timeout"`
}

// VaultAppRole configura il login AppRole.
type VaultAppRole struct {
	RoleID       string `yaml:"role_id"`
	SecretIDFile string `yaml:"secret_id_file"`
	Mount        string `yaml:"mount"`
}

// VaultK8s configura il login con il service account Kubernetes.
type VaultK8s struct {
	Role    string `yaml:"role"`
	JWTFile string `yaml:"jwt_file"`
	Mount   string `yaml:"mount"`
}

// Artifacts configura il download degli artefatti.
type Artifacts struct {
	// Timeout di un download (default 30m).
	Timeout time.Duration `yaml:"timeout"`
	// AllowUnlisted ammette sorgenti non elencate in Sources (default true).
	AllowUnlisted *bool            `yaml:"allow_unlisted"`
	Sources       []ArtifactSource `yaml:"sources"`
}

// ArtifactSource è una sorgente di artefatti, con eventuali credenziali.
type ArtifactSource struct {
	// Prefix dell'URL (https://, http:// o file:///).
	Prefix string        `yaml:"prefix"`
	Auth   *ArtifactAuth `yaml:"auth"`
	CAFile string        `yaml:"ca_file"`
}

// ArtifactAuth sono le credenziali di una sorgente: SecretRef è un riferimento del secret
// provider (es. vault:...), mai il valore.
type ArtifactAuth struct {
	Type      string `yaml:"type"`
	Username  string `yaml:"username"`
	Header    string `yaml:"header"`
	SecretRef string `yaml:"secret_ref"`
}

// Pipeline elenca gli step disponibili nell'ordine di esecuzione.
type Pipeline struct {
	Steps []Step `yaml:"steps"`
}

// Step definisce la configurazione statica di uno step.
type Step struct {
	Name string `yaml:"name"`
	Mode string `yaml:"mode"`
	// Enabled è il default per gli step opzionali, sovrascrivibile dal descrittore.
	Enabled    bool `yaml:"enabled"`
	MaxRetries int  `yaml:"max_retries"`
}

// Default restituisce una configurazione funzionante per lo sviluppo locale.
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
		// Il rollback è ammesso ma disattivato per le applicazioni; mai per le migration,
		// che di norma non sono reversibili.
		Rollback: Rollback{Locator: "plugin", ArtifactTypes: map[string]RollbackType{
			"application": {Allowed: true},
			"container":   {Allowed: true},
			"kubernetes":  {Allowed: true},
			"ecs":         {Allowed: true},
			"lambda":      {Allowed: true},
			"cloudrun":    {Allowed: true},
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

// applyDefaults completa le sezioni il cui default non può stare in Default(): YAML
// fonderebbe le mappe configurate con quelle di default invece di sostituirle.
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
}

// SecurityWarnings elenca le impostazioni adatte solo allo sviluppo.
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
	return w
}

// Load legge il file YAML (se indicato) sopra i default e applica le variabili d'ambiente.
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

// Validate verifica la coerenza della configurazione.
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

// validateOperations verifica drain, recovery, retention e osservabilità.
// validateSIEM verifica le destinazioni dei log strutturati.
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

// regionRe sono i nomi di region ammessi (come i nomi di ambiente).
var regionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// tokenHashRe è uno sha256 esadecimale.
var tokenHashRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// validateSecurity verifica autenticazione, segreti e sorgenti degli artefatti.
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
