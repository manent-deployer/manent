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

// Deactivator è implementato dagli Activator che sanno annullare l'attivazione, in caso di
// rollback (es. riportare il traffico sulla release precedente). Deve essere idempotente.
type Deactivator interface {
	Deactivate(ctx context.Context, in pipeline.Input) error
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

// Deactivate registra l'annullamento dell'attivazione senza altri effetti.
func (a LogActivator) Deactivate(ctx context.Context, in pipeline.Input) error {
	a.Logger.WarnContext(ctx, "attivazione annullata dal rollback",
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

// Compensate annulla l'attivazione se l'Activator lo supporta; altrimenti non fa nulla.
func (a Activation) Compensate(ctx context.Context, in pipeline.Input, _ pipeline.Output) (pipeline.Output, error) {
	d, ok := a.Activator.(Deactivator)
	if !ok {
		return pipeline.Output{pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "attivazione non annullabile"}, nil
	}
	if err := d.Deactivate(ctx, in); err != nil {
		return nil, err
	}
	return pipeline.Output{pipeline.OutRollback: pipeline.RollbackCompleted, "status": "deactivated"}, nil
}
