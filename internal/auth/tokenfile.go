package auth

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultTokenFileInterval is the minimum interval between two checks of the token file.
const DefaultTokenFileInterval = 5 * time.Second

// TokenFile authenticates the static tokens listed in a YAML file, reloaded when it changes: a
// token is added or revoked by editing the file, without restarting. If the changed file is
// invalid, the tokens read last time stay in use.
type TokenFile struct {
	Path string
	// Interval is the minimum time between two checks of the file (default DefaultTokenFileInterval).
	Interval time.Duration
	Logger   *slog.Logger

	mu      sync.Mutex
	tokens  Tokens
	checked time.Time
	modTime time.Time
	size    int64
}

// tokenFileDoc is the file format.
type tokenFileDoc struct {
	Tokens []struct {
		Name      string    `yaml:"name"`
		SHA256    string    `yaml:"sha256"`
		Groups    []string  `yaml:"groups"`
		ExpiresAt time.Time `yaml:"expires_at"`
	} `yaml:"tokens"`
}

// NewTokenFile reads the file: at startup an invalid file is an error.
func NewTokenFile(path string, logger *slog.Logger) (*TokenFile, error) {
	f := &TokenFile{Path: path, Logger: logger}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("token file: %w", err)
	}
	ts, err := ParseTokenFile(path)
	if err != nil {
		return nil, err
	}
	f.tokens, f.modTime, f.size, f.checked = ts, st.ModTime(), st.Size(), time.Now()
	return f, nil
}

// ParseTokenFile reads and validates a token file.
func ParseTokenFile(path string) (Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token file: %w", err)
	}
	var doc tokenFileDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && len(bytes.TrimSpace(data)) > 0 {
		return nil, fmt.Errorf("invalid token file %s: %w", path, err)
	}
	ts := make(Tokens, 0, len(doc.Tokens))
	names := map[string]bool{}
	for i, t := range doc.Tokens {
		if t.Name == "" || names[t.Name] || !validHash(t.SHA256) {
			return nil, fmt.Errorf("token file %s: tokens[%d] must have a unique name and a hex sha256", path, i)
		}
		names[t.Name] = true
		ts = append(ts, StaticToken{Name: t.Name, SHA256: t.SHA256, Groups: t.Groups, ExpiresAt: t.ExpiresAt})
	}
	return ts, nil
}

// validHash checks that s is a hexadecimal sha256.
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// Authenticate implements Authenticator.
func (f *TokenFile) Authenticate(ctx context.Context, token string) (*Principal, error) {
	return f.current().Authenticate(ctx, token)
}

// current returns the tokens, reloading the file if it changed since the last check.
func (f *TokenFile) current() Tokens {
	f.mu.Lock()
	defer f.mu.Unlock()
	interval := f.Interval
	if interval <= 0 {
		interval = DefaultTokenFileInterval
	}
	if time.Since(f.checked) < interval {
		return f.tokens
	}
	f.checked = time.Now()
	st, err := os.Stat(f.Path)
	if err != nil {
		// A removed file revokes all its tokens.
		if f.tokens != nil {
			f.Logger.Warn("token file unreadable: tokens revoked", "path", f.Path, "error", err)
		}
		f.tokens, f.modTime, f.size = nil, time.Time{}, 0
		return nil
	}
	if st.ModTime().Equal(f.modTime) && st.Size() == f.size {
		return f.tokens
	}
	ts, err := ParseTokenFile(f.Path)
	f.modTime, f.size = st.ModTime(), st.Size()
	if err != nil {
		f.Logger.Error("token file invalid: the previous tokens stay in use", "path", f.Path, "error", err)
		return f.tokens
	}
	f.tokens = ts
	f.Logger.Info("token file reloaded", "path", f.Path, "tokens", len(ts))
	return ts
}
