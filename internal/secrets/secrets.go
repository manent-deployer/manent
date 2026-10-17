// Package secrets resolves the secret references declared in the descriptor.
package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// Provider resolves a reference (e.g. "env:VAR", "file:/path") into the secret value.
type Provider interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// SchemeProvider dispatches references to the provider registered for the scheme (prefix
// before ':').
type SchemeProvider struct {
	providers map[string]Provider
}

// NewDefaultProvider creates a provider with the "env" and "file" schemes.
// Other backends (Vault, cloud KMS, ...) are added with Register.
func NewDefaultProvider() *SchemeProvider {
	p := &SchemeProvider{providers: map[string]Provider{}}
	p.Register("env", EnvProvider{})
	p.Register("file", FileProvider{})
	return p
}

// Register binds a provider to a scheme.
func (p *SchemeProvider) Register(scheme string, prov Provider) {
	p.providers[scheme] = prov
}

// Resolve finds the scheme and delegates to the matching provider.
func (p *SchemeProvider) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("secret reference %q without scheme", ref)
	}
	prov, ok := p.providers[scheme]
	if !ok {
		return "", fmt.Errorf("secret scheme %q not supported", scheme)
	}
	return prov.Resolve(ctx, rest)
}

// EnvProvider reads secrets from environment variables.
type EnvProvider struct{}

// Resolve returns the value of the given environment variable.
func (EnvProvider) Resolve(_ context.Context, name string) (string, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("environment variable %q not defined", name)
	}
	return v, nil
}

// FileProvider reads secrets from files.
type FileProvider struct{}

// Resolve returns the content of the given file.
func (FileProvider) Resolve(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DescriptorResolver exposes a descriptor's secrets to plugins by logical name.
// Values are resolved at every request and never stored.
type DescriptorResolver struct {
	Provider   Provider
	Descriptor *descriptor.Descriptor
}

// Secret resolves the secret with the given logical name.
func (r DescriptorResolver) Secret(ctx context.Context, name string) (string, error) {
	ref, ok := r.Descriptor.SecretRefByName(name)
	if !ok {
		return "", fmt.Errorf("secret %q not declared in the descriptor", name)
	}
	return r.Provider.Resolve(ctx, ref)
}
