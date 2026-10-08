package steps

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"

	"github.com/joeg-ita/go-dart/internal/artifact"
	"github.com/joeg-ita/go-dart/internal/pipeline"
)

// Chiavi dell'output dello step artifacts, lette dagli step successivi.
const (
	OutLocalPath = "local_path"
	OutSHA256    = "sha256"
	OutSize      = "size"
)

// Artifacts recupera l'artefatto nella directory di lavoro del deploy.
// Nota: con più executor DBOS la WorkDir deve essere su storage condiviso.
type Artifacts struct {
	Fetcher artifact.Fetcher
	WorkDir string
	Logger  *slog.Logger
}

// Name restituisce il nome dello step.
func (Artifacts) Name() string { return "artifacts" }

// Run scarica l'artefatto e ne restituisce percorso, hash e dimensione.
func (a Artifacts) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	dest := filepath.Join(a.WorkDir, in.DeploymentID)
	f, err := a.Fetcher.Fetch(ctx, in.Descriptor.Artifact.Source, dest)
	if err != nil {
		return nil, err
	}
	a.Logger.InfoContext(ctx, "artefatto recuperato", "deployment", in.DeploymentID, "path", f.LocalPath, "sha256", f.SHA256)
	return pipeline.Output{
		OutLocalPath: f.LocalPath,
		OutSHA256:    f.SHA256,
		OutSize:      strconv.FormatInt(f.Size, 10),
	}, nil
}
