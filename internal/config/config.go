// Package config gestisce la configurazione applicativa statica di go-dart.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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
	App      App                       `yaml:"app"`
	DBOS     DBOS                      `yaml:"dbos"`
	HTTP     HTTP                      `yaml:"http"`
	Logging  Logging                   `yaml:"logging"`
	SIEM     SIEM                      `yaml:"siem"`
	WorkDir  string                    `yaml:"workdir"`
	Pipeline Pipeline                  `yaml:"pipeline"`
	Plugins  map[string]map[string]any `yaml:"plugins"`
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
}

// HTTP configura il web server API.
type HTTP struct {
	Listen string `yaml:"listen"`
}

// Logging configura il log applicativo.
type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// SIEM configura l'invio dei log in formato RFC 5424.
type SIEM struct {
	Enabled bool   `yaml:"enabled"`
	Network string `yaml:"network"`
	Address string `yaml:"address"`
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
		App:     App{Name: "go-dart", Version: "0.1.0"},
		DBOS:    DBOS{DatabaseURL: "sqlite:go-dart.db", Queue: "deployments", Concurrency: 4, EnvironmentConcurrency: 1},
		HTTP:    HTTP{Listen: ":8080"},
		Logging: Logging{Level: "info", Format: "text"},
		SIEM:    SIEM{Network: "udp"},
		WorkDir: "./work",
		Pipeline: Pipeline{Steps: []Step{
			{Name: "secrets", Mode: ModeMandatory},
			{Name: "artifacts", Mode: ModeMandatory, MaxRetries: 3},
			{Name: "qa", Mode: ModeOptional, Enabled: true},
			{Name: "deploy", Mode: ModeMandatory, MaxRetries: 2},
			{Name: "post-test", Mode: ModeOptional, Enabled: false, MaxRetries: 5},
			{Name: "activation", Mode: ModeOptional, Enabled: true},
		}},
		Plugins: map[string]map[string]any{"remote-copy": {}},
	}
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
	if g := c.DBOS.GlobalConcurrency; g > 0 && c.DBOS.EnvironmentConcurrency > g {
		errs = append(errs, errors.New("dbos.environment_concurrency non può superare global_concurrency"))
	}
	seen := map[string]bool{}
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
