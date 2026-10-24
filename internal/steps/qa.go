package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/joeg-ita/manent/internal/pipeline"
)

// Checker is a single Quality Assurance check.
type Checker interface {
	Name() string
	Check(ctx context.Context, in pipeline.Input) error
}

// QA runs all configured checks in sequence and fails if at least one does not pass.
type QA struct {
	Checkers []Checker
	Logger   *slog.Logger
}

// Name returns the step name.
func (QA) Name() string { return "qa" }

// Run runs the checks and reports the outcome of each.
func (q QA) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	out := pipeline.Output{}
	var errs []error
	for _, c := range q.Checkers {
		if err := c.Check(ctx, in); err != nil {
			out[c.Name()] = "failed"
			errs = append(errs, fmt.Errorf("%s: %w", c.Name(), err))
			continue
		}
		out[c.Name()] = "passed"
	}
	if err := errors.Join(errs...); err != nil {
		q.Logger.WarnContext(ctx, "QA failed", "deployment", in.DeploymentID, "error", err)
		return nil, err
	}
	q.Logger.InfoContext(ctx, "QA passed", "deployment", in.DeploymentID)
	return out, nil
}

// DryRun is the same as Run: the checks only read the local artifact.
func (q QA) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return q.Run(ctx, in)
}

// ChecksumChecker compares the declared SHA-256 with the one computed by the artifacts step.
type ChecksumChecker struct{}

// Name returns the check name.
func (ChecksumChecker) Name() string { return "checksum" }

// Check passes if the descriptor declares no hash.
func (ChecksumChecker) Check(_ context.Context, in pipeline.Input) error {
	want := in.Descriptor.Artifact.SHA256
	if want == "" {
		return nil
	}
	got := in.Outputs["artifacts"][OutSHA256]
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("expected sha256 %s, computed %s", want, got)
	}
	return nil
}

// NonEmptyChecker checks that the artifact is not empty.
type NonEmptyChecker struct{}

// Name returns the check name.
func (NonEmptyChecker) Name() string { return "non-empty" }

// Check fails if the artifact has zero size or was not fetched.
func (NonEmptyChecker) Check(_ context.Context, in pipeline.Input) error {
	size := in.Outputs["artifacts"][OutSize]
	if size == "" || size == "0" {
		return errors.New("empty or not retrieved artifact")
	}
	return nil
}
