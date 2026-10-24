package steps

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/manent-deployer/manent/internal/artifact"
	"github.com/manent-deployer/manent/internal/pipeline"
)

// Output keys of the artifacts step, read by the next steps.
const (
	OutLocalPath = "local_path"
	OutSHA256    = "sha256"
	OutSize      = "size"
)

// ArtifactStore manages the artifacts in the worker's working directory.
type ArtifactStore interface {
	Fetch(ctx context.Context, deploymentID, source string) (*artifact.Fetched, error)
	// Ensure returns the local path of the artifact, downloading it again if missing on this
	// worker; the boolean reports whether it was downloaded again.
	Ensure(ctx context.Context, deploymentID, source, localPath, sha256 string) (string, bool, error)
}

// Artifacts fetches the artifact into the deploy's working directory.
type Artifacts struct {
	Store  ArtifactStore
	Logger *slog.Logger
}

// Name returns the step name.
func (Artifacts) Name() string { return "artifacts" }

// Run downloads the artifact and returns path, hash and size.
func (a Artifacts) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	f, err := a.Store.Fetch(ctx, in.DeploymentID, in.Descriptor.Artifact.Source)
	if err != nil {
		return nil, err
	}
	a.Logger.InfoContext(ctx, "artifact fetched", "deployment", in.DeploymentID, "path", f.LocalPath, "sha256", f.SHA256)
	return pipeline.Output{
		OutLocalPath: f.LocalPath,
		OutSHA256:    f.SHA256,
		OutSize:      strconv.FormatInt(f.Size, 10),
	}, nil
}

// DryRun is the same as Run: the artifact is downloaded only into the worker's working
// directory, which the final cleanup removes.
func (a Artifacts) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return a.Run(ctx, in)
}
