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

// DefaultTokenFileInterval è l'intervallo minimo tra due controlli del file dei token.
const DefaultTokenFileInterval = 5 * time.Second

// TokenFile autentica i token statici elencati in un file YAML, riletto quando cambia: un
// token si aggiunge o si revoca modificando il file, senza riavviare. Se il file modificato
// non è valido restano in uso i token letti l'ultima volta.
type TokenFile struct {
	Path string
	// Interval è il tempo minimo tra due controlli del file (default DefaultTokenFileInterval).
	Interval time.Duration
	Logger   *slog.Logger

	mu      sync.Mutex
	tokens  Tokens
	checked time.Time
	modTime time.Time
	size    int64
}

// tokenFileDoc è il formato del file.
type tokenFileDoc struct {
	Tokens []struct {
		Name      string    `yaml:"name"`
		SHA256    string    `yaml:"sha256"`
		Groups    []string  `yaml:"groups"`
		ExpiresAt time.Time `yaml:"expires_at"`
	} `yaml:"tokens"`
}

// NewTokenFile legge il file: all'avvio un file non valido è un errore.
func NewTokenFile(path string, logger *slog.Logger) (*TokenFile, error) {
	f := &TokenFile{Path: path, Logger: logger}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("file dei token: %w", err)
	}
	ts, err := ParseTokenFile(path)
	if err != nil {
		return nil, err
	}
	f.tokens, f.modTime, f.size, f.checked = ts, st.ModTime(), st.Size(), time.Now()
	return f, nil
}

// ParseTokenFile legge e verifica un file dei token.
func ParseTokenFile(path string) (Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("file dei token: %w", err)
	}
	var doc tokenFileDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && len(bytes.TrimSpace(data)) > 0 {
		return nil, fmt.Errorf("file dei token %s non valido: %w", path, err)
	}
	ts := make(Tokens, 0, len(doc.Tokens))
	names := map[string]bool{}
	for i, t := range doc.Tokens {
		if t.Name == "" || names[t.Name] || !validHash(t.SHA256) {
			return nil, fmt.Errorf("file dei token %s: tokens[%d] deve avere name univoco e sha256 esadecimale", path, i)
		}
		names[t.Name] = true
		ts = append(ts, StaticToken{Name: t.Name, SHA256: t.SHA256, Groups: t.Groups, ExpiresAt: t.ExpiresAt})
	}
	return ts, nil
}

// validHash verifica che s sia uno sha256 esadecimale.
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

// Authenticate implementa Authenticator.
func (f *TokenFile) Authenticate(ctx context.Context, token string) (*Principal, error) {
	return f.current().Authenticate(ctx, token)
}

// current restituisce i token, rileggendo il file se è cambiato dall'ultimo controllo.
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
		// Un file rimosso revoca tutti i suoi token.
		if f.tokens != nil {
			f.Logger.Warn("file dei token non leggibile: token revocati", "path", f.Path, "error", err)
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
		f.Logger.Error("file dei token non valido: restano in uso i token precedenti", "path", f.Path, "error", err)
		return f.tokens
	}
	f.tokens = ts
	f.Logger.Info("file dei token riletto", "path", f.Path, "tokens", len(ts))
	return ts
}
