// Package secrets risolve i riferimenti ai segreti dichiarati nel descrittore.
package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/joeg-ita/go-dart/internal/descriptor"
)

// Provider risolve un riferimento (es. "env:VAR", "file:/path") nel valore del segreto.
type Provider interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// SchemeProvider smista i riferimenti al provider registrato per lo schema (prefisso prima di ':').
type SchemeProvider struct {
	providers map[string]Provider
}

// NewDefaultProvider crea un provider con gli schemi "env" e "file".
// Altri backend (Vault, cloud KMS, ...) si aggiungono con Register.
func NewDefaultProvider() *SchemeProvider {
	p := &SchemeProvider{providers: map[string]Provider{}}
	p.Register("env", EnvProvider{})
	p.Register("file", FileProvider{})
	return p
}

// Register associa un provider a uno schema.
func (p *SchemeProvider) Register(scheme string, prov Provider) {
	p.providers[scheme] = prov
}

// Resolve individua lo schema e delega al provider corrispondente.
func (p *SchemeProvider) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("riferimento segreto %q senza schema", ref)
	}
	prov, ok := p.providers[scheme]
	if !ok {
		return "", fmt.Errorf("schema segreto %q non supportato", scheme)
	}
	return prov.Resolve(ctx, rest)
}

// EnvProvider legge i segreti da variabili d'ambiente.
type EnvProvider struct{}

// Resolve restituisce il valore della variabile d'ambiente indicata.
func (EnvProvider) Resolve(_ context.Context, name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("variabile d'ambiente %q non definita", name)
	}
	return v, nil
}

// FileProvider legge i segreti da file.
type FileProvider struct{}

// Resolve restituisce il contenuto del file indicato.
func (FileProvider) Resolve(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DescriptorResolver espone i segreti di un descrittore per nome logico ai plugin.
// I valori sono risolti a ogni richiesta e mai memorizzati.
type DescriptorResolver struct {
	Provider   Provider
	Descriptor *descriptor.Descriptor
}

// Secret risolve il segreto con il nome logico indicato.
func (r DescriptorResolver) Secret(ctx context.Context, name string) (string, error) {
	ref, ok := r.Descriptor.SecretRefByName(name)
	if !ok {
		return "", fmt.Errorf("segreto %q non dichiarato nel descrittore", name)
	}
	return r.Provider.Resolve(ctx, ref)
}
