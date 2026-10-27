// Package artifact fetches deploy artifacts into a local working directory.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Fetched describes an artifact downloaded locally.
type Fetched struct {
	LocalPath string `json:"local_path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// Fetcher fetches an artifact from the given source.
type Fetcher interface {
	Fetch(ctx context.Context, source, destDir string) (*Fetched, error)
}

// DefaultFetcher supports local paths, file://, http(s):// and registry images
// (oci://, oci+http://).
type DefaultFetcher struct {
	Client *http.Client
	// Sources, if set, limits the allowed sources and provides the credentials.
	Sources *Sources
}

// Fetch copies the artifact into destDir computing its SHA-256.
// Writing goes through a temporary file to stay idempotent across retries.
func (f DefaultFetcher) Fetch(ctx context.Context, source, destDir string) (*Fetched, error) {
	if IsOCI(source) {
		return f.fetchOCI(ctx, source, destDir)
	}
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
		return nil, fmt.Errorf("copying the artifact: %w", err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return nil, err
	}
	return &Fetched{LocalPath: dest, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// open opens the source and returns the destination file name.
func (f DefaultFetcher) open(ctx context.Context, source string) (io.ReadCloser, string, error) {
	if err := f.Sources.Check(source); err != nil {
		return nil, "", err
	}
	if isHTTP(source) {
		resp, err := f.httpDo(ctx, http.MethodGet, source)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, "", fmt.Errorf("artifact download: status %d", resp.StatusCode)
		}
		u, err := url.Parse(source)
		if err != nil {
			resp.Body.Close()
			return nil, "", err
		}
		return resp.Body, filepath.Base(u.Path), nil
	}
	path := strings.TrimPrefix(source, "file://")
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	return file, filepath.Base(path), nil
}

// isHTTP reports whether the source is an http(s) URL.
func isHTTP(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// httpDo sends a request to an http(s) source with the source credentials.
func (f DefaultFetcher) httpDo(ctx context.Context, method, source string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, source, nil)
	if err != nil {
		return nil, err
	}
	client, err := f.Sources.prepare(ctx, req, source)
	if err != nil {
		return nil, err
	}
	// Credentials do not follow a redirect to another host: net/http drops Authorization
	// and the sensitive headers when the domain changes.
	return f.client(client).Do(req)
}

// Store manages the artifacts of every deploy in the worker's local working directory.
// Every deploy uses the WorkDir/<deployment-id> subdirectory.
type Store struct {
	Fetcher Fetcher
	WorkDir string
}

// dir returns the working directory of a deploy.
func (s *Store) dir(deploymentID string) string {
	return filepath.Join(s.WorkDir, deploymentID)
}

// Fetch downloads the artifact into the deploy directory.
func (s *Store) Fetch(ctx context.Context, deploymentID, source string) (*Fetched, error) {
	return s.Fetcher.Fetch(ctx, source, s.dir(deploymentID))
}

// Ensure makes sure the artifact is on the current worker and returns its path.
// If the deploy was recovered on a worker other than the one that ran the artifacts step,
// the file is missing: it is downloaded again and must have the same recorded sha256.
func (s *Store) Ensure(ctx context.Context, deploymentID, source, localPath, sha256 string) (string, bool, error) {
	if localPath != "" {
		if _, err := os.Stat(localPath); err == nil {
			return localPath, false, nil
		}
	}
	f, err := s.Fetch(ctx, deploymentID, source)
	if err != nil {
		return "", false, fmt.Errorf("artifact missing on the worker, new download failed: %w", err)
	}
	if !strings.EqualFold(f.SHA256, sha256) {
		return "", false, fmt.Errorf("artifact downloaded again with sha256 %s, different from the recorded %s: the source has changed", f.SHA256, sha256)
	}
	return f.LocalPath, true, nil
}

// Cleanup removes the deploy's working directory from the current worker.
func (s *Store) Cleanup(_ context.Context, deploymentID string) error {
	if deploymentID == "" || strings.ContainsAny(deploymentID, `/\`) || deploymentID == "." || deploymentID == ".." {
		return fmt.Errorf("deploy ID %q cannot be used as a directory", deploymentID)
	}
	return os.RemoveAll(s.dir(deploymentID))
}
