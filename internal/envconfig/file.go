package envconfig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// FileProvider legge la configurazione da file YAML/JSON locali; utile per sviluppo e test.
type FileProvider struct {
	// PathTemplate è il modello del percorso, con segnaposto {application} e {environment}.
	PathTemplate string
}

// Get legge il file dell'applicazione/ambiente.
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
