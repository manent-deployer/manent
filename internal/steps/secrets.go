// Package steps holds the standard implementations of the pipeline steps.
package steps

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/manent-deployer/manent/internal/pipeline"
	"github.com/manent-deployer/manent/internal/secrets"
)

// Secrets checks that all declared secrets can be resolved.
// Values are not returned: plugins read them at use time through SecretResolver.
type Secrets struct {
	Provider secrets.Provider
	Logger   *slog.Logger
}

// Name returns the step name.
func (Secrets) Name() string { return "secrets" }

// Run resolves every reference to check it is available and checks that the secrets used by
// callback authentication are declared.
func (s Secrets) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	for i, cb := range in.Descriptor.Callbacks {
		if cb.Auth == nil {
			continue
		}
		if _, ok := in.Descriptor.SecretRefByName(cb.Auth.Secret); !ok {
			return nil, fmt.Errorf("callbacks[%d]: secret %q not declared in secrets", i, cb.Auth.Secret)
		}
	}
	for _, ref := range in.Descriptor.Secrets {
		if _, err := s.Provider.Resolve(ctx, ref.Ref); err != nil {
			return nil, fmt.Errorf("secret %q not available: %w", ref.Name, err)
		}
	}
	s.Logger.InfoContext(ctx, "secrets verified", "deployment", in.DeploymentID, "count", len(in.Descriptor.Secrets))
	return pipeline.Output{"count": strconv.Itoa(len(in.Descriptor.Secrets))}, nil
}

// DryRun is the same as Run: checking secrets has no effects.
func (s Secrets) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.Run(ctx, in)
}
