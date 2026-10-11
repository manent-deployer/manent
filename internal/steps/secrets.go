// Package steps contiene le implementazioni standard degli step della pipeline.
package steps

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
)

// Secrets verifica che tutti i segreti dichiarati siano risolvibili.
// I valori non vengono restituiti: i plugin li leggono al momento dell'uso tramite SecretResolver.
type Secrets struct {
	Provider secrets.Provider
	Logger   *slog.Logger
}

// Name restituisce il nome dello step.
func (Secrets) Name() string { return "secrets" }

// Run risolve ogni riferimento per verificarne la disponibilità e controlla che i segreti
// usati dall'autenticazione dei callback siano dichiarati.
func (s Secrets) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	for i, cb := range in.Descriptor.Callbacks {
		if cb.Auth == nil {
			continue
		}
		if _, ok := in.Descriptor.SecretRefByName(cb.Auth.Secret); !ok {
			return nil, fmt.Errorf("callbacks[%d]: segreto %q non dichiarato in secrets", i, cb.Auth.Secret)
		}
	}
	for _, ref := range in.Descriptor.Secrets {
		if _, err := s.Provider.Resolve(ctx, ref.Ref); err != nil {
			return nil, fmt.Errorf("segreto %q non disponibile: %w", ref.Name, err)
		}
	}
	s.Logger.InfoContext(ctx, "segreti verificati", "deployment", in.DeploymentID, "count", len(in.Descriptor.Secrets))
	return pipeline.Output{"count": strconv.Itoa(len(in.Descriptor.Secrets))}, nil
}

// DryRun coincide con Run: la verifica dei segreti non ha effetti.
func (s Secrets) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	return s.Run(ctx, in)
}
