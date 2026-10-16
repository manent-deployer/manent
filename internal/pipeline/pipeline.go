// Package pipeline defines the step contract and the resolution of the execution plan.
package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// Output is the result of a step. DBOS persists it: it must never contain secrets.
type Output map[string]string

// Input is what a step receives: the descriptor and the outputs of the steps already run.
type Input struct {
	DeploymentID string
	Descriptor   *descriptor.Descriptor
	Outputs      map[string]Output
}

// Step is a phase of the deploy pipeline.
// Run runs as a DBOS step: it must be idempotent because it can be repeated on retry or recovery.
type Step interface {
	Name() string
	Run(ctx context.Context, in Input) (Output, error)
}

// DryRunner is implemented by steps that can simulate their execution without effects on
// production environments. In a dry-run deploy the steps that do not implement it are skipped.
type DryRunner interface {
	DryRun(ctx context.Context, in Input) (Output, error)
}

// Compensator is implemented by steps with effects on the environments that can undo them.
// When a deploy fails and the rollback policy provides for it, the worker calls Compensate on
// the steps already started (the failed one included), in reverse order. Compensate receives
// the same Input as the step and the output of its Checkpoint, if any. It must be idempotent.
// The output reports the outcome in OutRollback (RollbackCompleted or RollbackSkipped).
type Compensator interface {
	Compensate(ctx context.Context, in Input, checkpoint Output) (Output, error)
}

// Checkpointer is implemented by Compensators that must record, before running the step, the
// state to go back to (e.g. the release in service). The checkpoint is a separate DBOS step:
// retries of the step do not repeat it, so it cannot read an already changed state.
// It must have no effects: it also runs in dry-run.
type Checkpointer interface {
	Checkpoint(ctx context.Context, in Input) (Output, error)
}

// Key and outcomes of a compensation.
const (
	OutRollback       = "rollback"
	RollbackCompleted = "completed"
	// RollbackSkipped means there was nothing to restore (e.g. first deploy).
	RollbackSkipped = "skipped"
)

// ErrPermanent marks the errors a retry cannot fix (e.g. a policy violation or an invalid
// request): the step fails at once, without DBOS retries.
var ErrPermanent = errors.New("errore permanente")

// permanentError keeps the message of the original error.
type permanentError struct{ err error }

func (e permanentError) Error() string        { return e.err.Error() }
func (e permanentError) Unwrap() error        { return e.err }
func (e permanentError) Is(target error) bool { return target == ErrPermanent }

// Permanent marks err as permanent; nil stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// Retryable is the retry predicate of the steps: permanent errors are not retried.
func Retryable(err error) bool { return !errors.Is(err, ErrPermanent) }

// Cleaner frees a deploy's local resources at the end of the workflow (e.g. the working directory).
type Cleaner interface {
	Cleanup(ctx context.Context, deploymentID string) error
}

// Registry holds the available steps by name.
type Registry struct {
	steps map[string]Step
}

// NewRegistry creates a registry with the given steps.
func NewRegistry(steps ...Step) (*Registry, error) {
	r := &Registry{steps: map[string]Step{}}
	for _, s := range steps {
		if _, ok := r.steps[s.Name()]; ok {
			return nil, fmt.Errorf("step %q registrato due volte", s.Name())
		}
		r.steps[s.Name()] = s
	}
	return r, nil
}

// Get returns the step with the given name.
func (r *Registry) Get(name string) (Step, bool) {
	s, ok := r.steps[name]
	return s, ok
}
