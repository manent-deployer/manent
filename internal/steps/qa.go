package steps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// Checker è un singolo controllo di Quality Assurance.
type Checker interface {
	Name() string
	Check(ctx context.Context, in pipeline.Input) error
}

// QA esegue in sequenza tutti i controlli configurati e fallisce se almeno uno non passa.
type QA struct {
	Checkers []Checker
	Logger   *slog.Logger
}

// Name restituisce il nome dello step.
func (QA) Name() string { return "qa" }

// Run esegue i controlli e riporta l'esito di ciascuno.
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
		q.Logger.WarnContext(ctx, "QA fallita", "deployment", in.DeploymentID, "error", err)
		return nil, err
	}
	q.Logger.InfoContext(ctx, "QA superata", "deployment", in.DeploymentID)
	return out, nil
}

// ChecksumChecker confronta lo SHA-256 dichiarato con quello calcolato dallo step artifacts.
type ChecksumChecker struct{}

// Name restituisce il nome del controllo.
func (ChecksumChecker) Name() string { return "checksum" }

// Check passa se nel descrittore non è dichiarato alcun hash.
func (ChecksumChecker) Check(_ context.Context, in pipeline.Input) error {
	want := in.Descriptor.Artifact.SHA256
	if want == "" {
		return nil
	}
	got := in.Outputs["artifacts"][OutSHA256]
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("sha256 atteso %s, calcolato %s", want, got)
	}
	return nil
}

// NonEmptyChecker verifica che l'artefatto non sia vuoto.
type NonEmptyChecker struct{}

// Name restituisce il nome del controllo.
func (NonEmptyChecker) Name() string { return "non-empty" }

// Check fallisce se l'artefatto ha dimensione zero o non è stato recuperato.
func (NonEmptyChecker) Check(_ context.Context, in pipeline.Input) error {
	size := in.Outputs["artifacts"][OutSize]
	if size == "" || size == "0" {
		return errors.New("artefatto vuoto o non recuperato")
	}
	return nil
}
