// Package app compone tutti i componenti di go-dart a partire dalla configurazione.
// È l'unico punto in cui le implementazioni concrete vengono collegate alle interfacce.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

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
	Config  *config.Config
	Logger  *slog.Logger
	Service *engine.Engine
	worker  *engine.Worker // valorizzato in modalità worker
	client  dbos.Client    // valorizzato in modalità client
	siem    logging.SIEMSender
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

// NewSteps crea il registry degli step standard. Lo store degli artefatti è condiviso
// tra lo step artifacts, lo step deploy (che lo riscarica se manca) e la pulizia finale.
func NewSteps(plugins *plugin.Registry, store *artifact.Store, log *slog.Logger) (*pipeline.Registry, error) {
	sp := secrets.NewDefaultProvider()
	stepLog := log.With("component", "step")
	return pipeline.NewRegistry(
		steps.Secrets{Provider: sp, Logger: stepLog},
		steps.Artifacts{Store: store, Logger: stepLog},
		steps.QA{Checkers: []steps.Checker{steps.NonEmptyChecker{}, steps.ChecksumChecker{}}, Logger: stepLog},
		steps.Deploy{
			Policies:  map[string]steps.Policy{"": steps.DefaultPolicy{Plugins: plugins}, "default": steps.DefaultPolicy{Plugins: plugins}},
			Secrets:   sp,
			Artifacts: store,
			Logger:    stepLog,
		},
		steps.PostTest{Client: &http.Client{Timeout: 10 * time.Second}, Logger: stepLog},
		steps.Activation{Activator: steps.LogActivator{Logger: stepLog}},
	)
}

// components contiene i componenti comuni a worker e client.
type components struct {
	log     *slog.Logger
	siem    logging.SIEMSender
	steps   *pipeline.Registry
	store   *artifact.Store
	planner pipeline.Planner
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
	store := &artifact.Store{Fetcher: artifact.DefaultFetcher{}, WorkDir: cfg.WorkDir}
	reg, err := NewSteps(plugins, store, log)
	if err != nil {
		return nil, err
	}
	return &components{
		log:     log,
		siem:    sender,
		steps:   reg,
		store:   store,
		planner: pipeline.ConfigPlanner{Steps: cfg.Pipeline.Steps, Registry: reg},
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
		Cleaner:              c.store,
		Logger:               c.log,
		SchedulePollInterval: cfg.DBOS.SchedulePollInterval,
	})
	if err != nil {
		return nil, err
	}
	if err := w.Launch(); err != nil {
		return nil, fmt.Errorf("avvio DBOS: %w", err)
	}
	return &App{
		Config:  cfg,
		Logger:  c.log,
		Service: engine.NewService(engine.ServiceOptions{Client: w.Client(), QueueName: cfg.DBOS.Queue, Planner: c.planner, Logger: c.log}),
		worker:  w,
		siem:    c.siem,
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
	return &App{
		Config:  cfg,
		Logger:  c.log,
		Service: engine.NewService(engine.ServiceOptions{Client: client, QueueName: cfg.DBOS.Queue, Planner: c.planner, Logger: c.log}),
		client:  client,
		siem:    c.siem,
	}, nil
}

// Close arresta il worker (o il client) DBOS e chiude la connessione al SIEM.
func (a *App) Close() {
	var err error
	if a.worker != nil {
		err = a.worker.Shutdown(30 * time.Second)
	} else {
		err = dbos.Shutdown(a.client, 5*time.Second)
	}
	if err != nil {
		a.Logger.Error("shutdown DBOS", "error", err)
	}
	_ = a.siem.Close()
}
