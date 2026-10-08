// Package app compone tutti i componenti di go-dart a partire dalla configurazione.
// È l'unico punto in cui le implementazioni concrete vengono collegate alle interfacce.
package app

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/joeg-ita/go-dart/internal/artifact"
	"github.com/joeg-ita/go-dart/internal/config"
	"github.com/joeg-ita/go-dart/internal/engine"
	"github.com/joeg-ita/go-dart/internal/logging"
	"github.com/joeg-ita/go-dart/internal/pipeline"
	"github.com/joeg-ita/go-dart/internal/secrets"
	"github.com/joeg-ita/go-dart/internal/steps"
	"github.com/joeg-ita/go-dart/pkg/plugin"
	"github.com/joeg-ita/go-dart/plugins/remotecopy"
)

// App contiene i componenti condivisi da CLI e API.
type App struct {
	Config *config.Config
	Logger *slog.Logger
	Engine *engine.Engine
	siem   logging.SIEMSender
}

// NewLogger crea il logger applicativo con l'eventuale inoltro al SIEM.
func NewLogger(cfg *config.Config) (*slog.Logger, logging.SIEMSender, error) {
	var sender logging.SIEMSender = logging.NoopSender{}
	if cfg.SIEM.Enabled {
		s, err := logging.NewNetSender(cfg.SIEM.Network, cfg.SIEM.Address)
		if err != nil {
			return nil, nil, err
		}
		sender = s
	}
	log := logging.New(logging.Options{
		AppName: cfg.App.Name,
		Level:   cfg.Logging.Level,
		Format:  cfg.Logging.Format,
		SIEM:    sender,
	})
	return log, sender, nil
}

// NewPlugins registra i plugin disponibili e attiva quelli presenti in configurazione.
// I plugin di altri team vanno aggiunti qui con Register.
func NewPlugins(cfg *config.Config, log *slog.Logger) (*plugin.Registry, error) {
	reg := plugin.NewRegistry()
	if err := reg.Register(remotecopy.Name, remotecopy.Factory); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Plugins)) {
		if err := reg.Activate(name, cfg.Plugins[name], log); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// NewSteps crea il registry degli step standard.
func NewSteps(cfg *config.Config, plugins *plugin.Registry, log *slog.Logger) (*pipeline.Registry, error) {
	sp := secrets.NewDefaultProvider()
	stepLog := log.With("component", "step")
	return pipeline.NewRegistry(
		steps.Secrets{Provider: sp, Logger: stepLog},
		steps.Artifacts{Fetcher: artifact.DefaultFetcher{}, WorkDir: cfg.WorkDir, Logger: stepLog},
		steps.QA{Checkers: []steps.Checker{steps.NonEmptyChecker{}, steps.ChecksumChecker{}}, Logger: stepLog},
		steps.Deploy{
			Policies: map[string]steps.Policy{"": steps.DefaultPolicy{Plugins: plugins}, "default": steps.DefaultPolicy{Plugins: plugins}},
			Secrets:  sp,
			Logger:   stepLog,
		},
		steps.PostTest{Client: &http.Client{Timeout: 10 * time.Second}, Logger: stepLog},
		steps.Activation{Activator: steps.LogActivator{Logger: stepLog}},
	)
}

// New costruisce l'applicazione completa e avvia DBOS.
func New(cfg *config.Config) (*App, error) {
	log, sender, err := NewLogger(cfg)
	if err != nil {
		return nil, err
	}
	plugins, err := NewPlugins(cfg, log)
	if err != nil {
		return nil, err
	}
	reg, err := NewSteps(cfg, plugins, log)
	if err != nil {
		return nil, err
	}
	eng, err := engine.New(engine.Options{
		AppName:     cfg.App.Name,
		AppVersion:  cfg.App.Version,
		DatabaseURL: cfg.DBOS.DatabaseURL,
		QueueName:   cfg.DBOS.Queue,
		Concurrency: cfg.DBOS.Concurrency,
		Planner:     pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg},
		Steps:       reg,
		Logger:      log,
	})
	if err != nil {
		return nil, err
	}
	if err := eng.Launch(); err != nil {
		return nil, fmt.Errorf("avvio DBOS: %w", err)
	}
	return &App{Config: cfg, Logger: log, Engine: eng, siem: sender}, nil
}

// Close arresta DBOS e chiude la connessione al SIEM.
func (a *App) Close() {
	if err := a.Engine.Shutdown(30 * time.Second); err != nil {
		a.Logger.Error("shutdown DBOS", "error", err)
	}
	_ = a.siem.Close()
}
