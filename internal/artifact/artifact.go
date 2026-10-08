// Package artifact recupera gli artefatti di deploy in una directory di lavoro locale.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Fetched descrive un artefatto scaricato in locale.
type Fetched struct {
	LocalPath string `json:"local_path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// Fetcher recupera un artefatto dalla sorgente indicata.
type Fetcher interface {
	Fetch(ctx context.Context, source, destDir string) (*Fetched, error)
}

// DefaultFetcher supporta percorsi locali, file:// e http(s)://.
type DefaultFetcher struct {
	Client *http.Client
}

// Fetch copia l'artefatto in destDir calcolandone lo SHA-256.
// La scrittura passa da un file temporaneo per restare idempotente in caso di retry.
func (f DefaultFetcher) Fetch(ctx context.Context, source, destDir string) (*Fetched, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	src, name, err := f.open(ctx, source)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	dest := filepath.Join(destDir, name)
	tmp, err := os.CreateTemp(destDir, ".fetch-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("copia artefatto: %w", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return nil, err
	}
	return &Fetched{LocalPath: dest, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// open apre la sorgente e restituisce il nome file di destinazione.
func (f DefaultFetcher) open(ctx context.Context, source string) (io.ReadCloser, string, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		client := f.Client
		if client == nil {
			client = http.DefaultClient
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, "", fmt.Errorf("download artefatto: status %d", resp.StatusCode)
		}
		return resp.Body, filepath.Base(req.URL.Path), nil
	}
	path := strings.TrimPrefix(source, "file://")
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	return file, filepath.Base(path), nil
}
