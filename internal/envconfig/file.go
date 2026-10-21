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
}

// Get reads the file of the artifact in the application/environment.
func (p *FileProvider) Get(_ context.Context, application, artifact, environment string) (*Document, error) {
	return readKey(p.PathTemplate, application, artifact, environment, p.read)
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
