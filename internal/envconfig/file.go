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
	// PathTemplate is the path template, with {application} and {environment} placeholders.
	PathTemplate string
	// TenantTemplate is the path template of the tenant documents, with {tenant} (empty: no
	// tenant registry).
	TenantTemplate string
}

// Get reads the application/environment file.
func (p *FileProvider) Get(_ context.Context, application, environment string) (*Document, error) {
	path := expandKey(p.PathTemplate, application, environment)
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
