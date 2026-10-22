package envconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/joeg-ita/manent/internal/gitsrc"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// GitRepo reads files at a revision of a Git repository (gitsrc.Repo).
type GitRepo interface {
	Resolve(ctx context.Context, want string, pinned bool) (gitsrc.Commit, error)
	Read(ctx context.Context, sha, path string) ([]byte, error)
	URL() string
}

// GitProvider reads the configuration from a Git repository at a revision: the ref (default or
// requested) is resolved into a commit SHA, recorded as the document revision, so a deploy can be
// rebuilt from artifact sha256 + repository@sha:path.
type GitProvider struct {
	Repo GitRepo
	// PathTemplate is the path in the repository, with {application}, {artifact} and
	// {environment}; it never comes from the request.
	PathTemplate string
}

// Get reads the configuration at the default ref.
func (p *GitProvider) Get(ctx context.Context, application, artifact, environment string) (*Document, error) {
	return p.GetAt(ctx, application, artifact, environment, Revision{})
}

// GetAt reads the configuration at the requested revision.
func (p *GitProvider) GetAt(ctx context.Context, application, artifact, environment string, rev Revision) (*Document, error) {
	c, err := p.Repo.Resolve(ctx, rev.Ref, rev.Pinned)
	if err != nil {
		return nil, err
	}
	return readKey(p.PathTemplate, application, artifact, environment, func(path string) (*Document, error) {
		source := "git:" + p.Repo.URL() + "//" + path
		data, err := p.Repo.Read(ctx, c.SHA, path)
		if errors.Is(err, gitsrc.ErrNotFound) {
			// A missing file at a pinned or requested revision is not "no configuration": the
			// deploy would run without the environment's constraints.
			if rev.Ref != "" {
				return nil, pipeline.Permanent(fmt.Errorf("environment configuration %s not found at %s", source, c.SHA))
			}
			return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, source, c.SHA)
		}
		if err != nil {
			return nil, err
		}
		cfg, err := Parse(data)
		if err != nil {
			return nil, pipeline.Permanent(fmt.Errorf("%s@%s: %w", source, c.SHA, err))
		}
		return &Document{Config: cfg, Source: source, Revision: c.SHA, Ref: c.Ref}, nil
	})
}
