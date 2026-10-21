package envconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// FileProvider reads the configuration from local YAML/JSON files; useful for development and tests.
type FileProvider struct {
	// PathTemplate is the path template, with {application}, {artifact} and {environment}
	// placeholders.
	PathTemplate string
	// LegacyTemplate, if set, is read when the PathTemplate document is missing: the
	// per-application template used before {artifact}, kept only during the transition.
	LegacyTemplate string
	// TenantTemplate is the path template of the tenant documents, with {tenant} (empty: no
	// tenant registry).
	TenantTemplate string
}

// Get reads the file of the artifact in the application/environment.
func (p *FileProvider) Get(_ context.Context, application, artifact, environment string) (*Document, error) {
	return withLegacy(p.PathTemplate, p.LegacyTemplate, artifact, func(tmpl string) (*Document, error) {
		return p.read(expandKey(tmpl, application, artifact, environment))
	})
}

// read reads and decodes a configuration file.
func (p *FileProvider) read(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: file:%s", ErrNotFound, path)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("file:%s: %w", path, err)
	}
	return &Document{Config: cfg, Source: "file:" + path}, nil
}
