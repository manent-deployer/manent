package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/bundle"
)

// FileSource reads the revision from a local directory (e.g. a Kubernetes ConfigMap): *.rego
// and data.json/data.yaml files, hidden files and directories excluded. It is read again only
// when names, sizes or modification times change.
type FileSource struct {
	Dir string

	mu  sync.Mutex
	sig string
}

// Fetch implements Source.
func (s *FileSource) Fetch(context.Context) (Files, string, bool, error) {
	type entry struct {
		rel string
		fi  fs.FileInfo
	}
	var entries []entry
	err := filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != s.Dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".rego") && !isData(rel) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel, fi})
		return nil
	})
	if err != nil {
		return nil, "", false, err
	}
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s|%d|%d\n", e.rel, e.fi.Size(), e.fi.ModTime().UnixNano())
	}
	sig := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig == s.sig {
		return nil, "", false, nil
	}
	files := Files{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(e.rel)))
		if err != nil {
			return nil, "", false, err
		}
		files[e.rel] = string(b)
	}
	if len(files) == 0 {
		return nil, "", false, fmt.Errorf("no policy file (*.rego) in %s", s.Dir)
	}
	s.sig = sig
	return files, s.Dir, true, nil
}

// Permissions reports a directory or file others can write: anyone could change the policy.
func (s *FileSource) Permissions() []string {
	var out []string
	_ = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().Perm()&0o002 != 0 {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// BundleSource downloads an OPA bundle (tar.gz) over HTTP(S), with the ETag to download it
// only when it changes, and verifies its signature (.signatures.json).
type BundleSource struct {
	URL    string
	Client *http.Client
	// Token, if set, returns the bearer token of the request (resolved at every download).
	Token func(ctx context.Context) (string, error)
	// Verification, if nil, accepts unsigned bundles (only with an explicit configuration).
	Verification *bundle.VerificationConfig

	mu   sync.Mutex
	etag string
}

// NewVerification returns the verification of the bundle signed with the public key (PEM, or
// the HMAC secret for HS256).
func NewVerification(key, algorithm, keyID, scope string) *bundle.VerificationConfig {
	if keyID == "" {
		keyID = "default"
	}
	if algorithm == "" {
		algorithm = "RS256"
	}
	return bundle.NewVerificationConfig(map[string]*bundle.KeyConfig{keyID: {Key: key, Algorithm: algorithm, Scope: scope}}, keyID, scope, nil)
}

// Fetch implements Source.
func (s *BundleSource) Fetch(ctx context.Context) (Files, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, "", false, err
	}
	s.mu.Lock()
	etag := s.etag
	s.mu.Unlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if s.Token != nil {
		tok, err := s.Token(ctx)
		if err != nil {
			return nil, "", false, fmt.Errorf("bundle credentials: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, "", false, nil
	case http.StatusOK:
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", false, fmt.Errorf("bundle %s: status %d: %s", s.URL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*MaxRevisionSize+1))
	if err != nil {
		return nil, "", false, err
	}
	if len(raw) > 2*MaxRevisionSize {
		return nil, "", false, fmt.Errorf("bundle %s too large", s.URL)
	}
	files, revision, err := ReadBundle(raw, s.Verification)
	if err != nil {
		return nil, "", false, fmt.Errorf("bundle %s: %w", s.URL, err)
	}
	s.mu.Lock()
	s.etag = resp.Header.Get("ETag")
	s.mu.Unlock()
	return files, revision, true, nil
}

// ReadBundle reads a bundle (tar.gz), verifying the signature if vc is set, and returns its
// files and the manifest revision.
func ReadBundle(raw []byte, vc *bundle.VerificationConfig) (Files, string, error) {
	r := bundle.NewReader(bytes.NewReader(raw)).WithRegoVersion(ast.RegoV1)
	if vc != nil {
		r = r.WithBundleVerificationConfig(vc)
	} else {
		r = r.WithSkipBundleVerification(true)
	}
	b, err := r.Read()
	if err != nil {
		return nil, "", err
	}
	if vc != nil && len(b.Signatures.Signatures) == 0 {
		return nil, "", errors.New("unsigned bundle: the signature (.signatures.json) is required")
	}
	files := Files{}
	for _, m := range b.Modules {
		files[strings.TrimPrefix(filepath.ToSlash(m.Path), "/")] = string(m.Raw)
	}
	if len(b.Data) > 0 {
		data, err := json.Marshal(b.Data)
		if err != nil {
			return nil, "", err
		}
		files["data.json"] = string(data)
	}
	return files, b.Manifest.Revision, nil
}

// ConsulSource reads the revision from the Consul KV store: every key under Prefix is a file
// (the rest of the key is its path), read again when the index of the prefix changes.
type ConsulSource struct {
	Address    string
	Token      string
	Datacenter string
	Prefix     string
	Client     *http.Client

	mu    sync.Mutex
	index string
}

// Fetch implements Source.
func (s *ConsulSource) Fetch(ctx context.Context) (Files, string, bool, error) {
	prefix := strings.Trim(s.Prefix, "/") + "/"
	q := url.Values{"recurse": {"true"}}
	if s.Datacenter != "" {
		q.Set("dc", s.Datacenter)
	}
	u := strings.TrimRight(s.Address, "/") + "/v1/kv/" + prefix + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", false, err
	}
	if s.Token != "" {
		req.Header.Set("X-Consul-Token", s.Token)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("reading from Consul: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, "", false, fmt.Errorf("no policy under the Consul prefix %s", prefix)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", false, fmt.Errorf("reading from Consul %s: status %d: %s", prefix, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var kvs []struct {
		Key         string
		Value       string
		ModifyIndex uint64
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*MaxRevisionSize)).Decode(&kvs); err != nil {
		return nil, "", false, fmt.Errorf("invalid Consul response: %w", err)
	}
	var max uint64
	for _, kv := range kvs {
		max = slices.Max([]uint64{max, kv.ModifyIndex})
	}
	index := cmpIndex(resp.Header.Get("X-Consul-Index"), max)
	s.mu.Lock()
	defer s.mu.Unlock()
	if index == s.index {
		return nil, "", false, nil
	}
	files := Files{}
	for _, kv := range kvs {
		rel := strings.TrimPrefix(kv.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		v, err := base64.StdEncoding.DecodeString(kv.Value)
		if err != nil {
			return nil, "", false, fmt.Errorf("invalid Consul value %s: %w", kv.Key, err)
		}
		files[rel] = string(v)
	}
	if len(files) == 0 {
		return nil, "", false, fmt.Errorf("no policy under the Consul prefix %s", prefix)
	}
	s.index = index
	return files, "consul:" + index, true, nil
}

// cmpIndex prefers the X-Consul-Index header, falling back to the highest ModifyIndex.
func cmpIndex(header string, max uint64) string {
	if header != "" {
		return header
	}
	return strconv.FormatUint(max, 10)
}
