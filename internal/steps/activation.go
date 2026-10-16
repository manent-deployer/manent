package steps

import (
	"context"
	"log/slog"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// Activator puts the release into service (traffic switch, change opening, ...).
type Activator interface {
	Activate(ctx context.Context, in pipeline.Input) error
}

// Deactivator is implemented by Activators that can undo the activation, on rollback (e.g.
// bring traffic back to the previous release). It must be idempotent.
type Deactivator interface {
	Deactivate(ctx context.Context, in pipeline.Input) error
}

// LogActivator is the initial implementation: it only logs the activation.
type LogActivator struct {
	Logger *slog.Logger
}

// Activate logs the activation without other effects.
func (a LogActivator) Activate(ctx context.Context, in pipeline.Input) error {
	a.Logger.InfoContext(ctx, "rilascio attivato in esercizio",
		"deployment", in.DeploymentID,
		"application", in.Descriptor.Application,
		"environment", in.Descriptor.Environment,
		"version", in.Descriptor.Artifact.Version)
	return nil
}

// Deactivate logs the undoing of the activation without other effects.
func (a LogActivator) Deactivate(ctx context.Context, in pipeline.Input) error {
	a.Logger.WarnContext(ctx, "attivazione annullata dal rollback",
		"deployment", in.DeploymentID,
		"application", in.Descriptor.Application,
		"environment", in.Descriptor.Environment,
		"version", in.Descriptor.Artifact.Version)
	return nil
}

// Activation delegates to the configured Activator.
type Activation struct {
	Activator Activator
}

// Name returns the step name.
func (Activation) Name() string { return "activation" }

// Run runs the activation.
func (a Activation) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if err := a.Activator.Activate(ctx, in); err != nil {
		return nil, err
	}
	return pipeline.Output{"status": "active"}, nil
}

// Compensate undoes the activation if the Activator supports it; otherwise it does nothing.
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
