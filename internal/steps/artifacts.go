package steps

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/joeg-ita/manent/internal/artifact"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// Chiavi dell'output dello step artifacts, lette dagli step successivi.
const (
	OutLocalPath = "local_path"
	OutSHA256    = "sha256"
	OutSize      = "size"
)

// ArtifactStore gestisce gli artefatti nella directory di lavoro del worker.
type ArtifactStore interface {
	Fetch(ctx context.Context, deploymentID, source string) (*artifact.Fetched, error)
	// Ensure restituisce il percorso locale dell'artefatto, riscaricandolo se manca su questo worker;
	// il booleano indica se è stato riscaricato.
	Ensure(ctx context.Context, deploymentID, source, localPath, sha256 string) (string, bool, error)
}

// Artifacts recupera l'artefatto nella directory di lavoro del deploy.
type Artifacts struct {
	Store  ArtifactStore
	Logger *slog.Logger
}

// Name restituisce il nome dello step.
func (Artifacts) Name() string { return "artifacts" }

// Run scarica l'artefatto e ne restituisce percorso, hash e dimensione.
func (a Artifacts) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	f, err := a.Store.Fetch(ctx, in.DeploymentID, in.Descriptor.Artifact.Source)
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
