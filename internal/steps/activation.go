package steps

import (
	"context"
	"log/slog"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// Activator rende operativo in esercizio il rilascio (switch del traffico, apertura change, ...).
type Activator interface {
	Activate(ctx context.Context, in pipeline.Input) error
}

// LogActivator è l'implementazione iniziale: registra soltanto l'attivazione.
type LogActivator struct {
	Logger *slog.Logger
}

// Activate registra l'attivazione senza altri effetti.
func (a LogActivator) Activate(ctx context.Context, in pipeline.Input) error {
	a.Logger.InfoContext(ctx, "rilascio attivato in esercizio",
		"deployment", in.DeploymentID,
		"application", in.Descriptor.Application,
		"environment", in.Descriptor.Environment,
		"version", in.Descriptor.Artifact.Version)
	return nil
}

// Activation delega all'Activator configurato.
type Activation struct {
	Activator Activator
}

// Name restituisce il nome dello step.
func (Activation) Name() string { return "activation" }

// Run esegue l'attivazione.
func (a Activation) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if err := a.Activator.Activate(ctx, in); err != nil {
		return nil, err
	}
	return pipeline.Output{"status": "active"}, nil
}
