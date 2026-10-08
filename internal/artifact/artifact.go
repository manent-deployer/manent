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

// Store gestisce gli artefatti di ogni deploy nella directory di lavoro locale del worker.
// Ogni deploy usa la sottodirectory WorkDir/<deployment-id>.
type Store struct {
	Fetcher Fetcher
	WorkDir string
}

// dir restituisce la directory di lavoro di un deploy.
func (s *Store) dir(deploymentID string) string {
	return filepath.Join(s.WorkDir, deploymentID)
}

// Fetch scarica l'artefatto nella directory del deploy.
func (s *Store) Fetch(ctx context.Context, deploymentID, source string) (*Fetched, error) {
	return s.Fetcher.Fetch(ctx, source, s.dir(deploymentID))
}

// Ensure garantisce che l'artefatto sia presente sul worker corrente e ne restituisce il percorso.
// Se il deploy è stato recuperato su un worker diverso da quello che ha eseguito lo step
// artifacts, il file non c'è: viene riscaricato e deve avere lo stesso sha256 registrato.
func (s *Store) Ensure(ctx context.Context, deploymentID, source, localPath, sha256 string) (string, bool, error) {
	if localPath != "" {
		if _, err := os.Stat(localPath); err == nil {
			return localPath, false, nil
		}
	}
	f, err := s.Fetch(ctx, deploymentID, source)
	if err != nil {
		return "", false, fmt.Errorf("artefatto assente sul worker, nuovo download fallito: %w", err)
	}
	if !strings.EqualFold(f.SHA256, sha256) {
		return "", false, fmt.Errorf("artefatto riscaricato con sha256 %s diverso da quello registrato %s: la sorgente è cambiata", f.SHA256, sha256)
	}
	return f.LocalPath, true, nil
}

// Cleanup rimuove la directory di lavoro del deploy dal worker corrente.
func (s *Store) Cleanup(_ context.Context, deploymentID string) error {
	if deploymentID == "" || strings.ContainsAny(deploymentID, `/\`) || deploymentID == "." || deploymentID == ".." {
		return fmt.Errorf("ID di deploy %q non utilizzabile come directory", deploymentID)
	}
	return os.RemoveAll(s.dir(deploymentID))
}
