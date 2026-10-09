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

// Run risolve ogni riferimento per verificarne la disponibilità.
func (s Secrets) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	for _, ref := range in.Descriptor.Secrets {
		if _, err := s.Provider.Resolve(ctx, ref.Ref); err != nil {
			return nil, fmt.Errorf("segreto %q non disponibile: %w", ref.Name, err)
		}
	}
	s.Logger.InfoContext(ctx, "segreti verificati", "deployment", in.DeploymentID, "count", len(in.Descriptor.Secrets))
	return pipeline.Output{"count": strconv.Itoa(len(in.Descriptor.Secrets))}, nil
}
