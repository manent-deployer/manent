package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/joeg-ita/manent/internal/audit"
	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/customspec"
	"github.com/joeg-ita/manent/internal/customstep"
	"github.com/joeg-ita/manent/internal/governance"
	"github.com/joeg-ita/manent/internal/secrets"
)

// NewSpecKeys risolve le chiavi che firmano le specifiche degli step personalizzati. Le
// usano sia chi accoda (firma nel piano) sia il worker (verifica), quindi vanno lette
// all'avvio da entrambi.
func NewSpecKeys(cfg *config.Config, sp secrets.Provider) (customspec.Keys, error) {
	var keys customspec.Keys
	for i, ref := range cfg.Pipeline.Custom.SpecKeyRefs {
		v, err := sp.Resolve(context.Background(), ref)
		if err != nil {
			return nil, fmt.Errorf("pipeline.custom.spec_key_refs[%d]: %w", i, err)
		}
		if len(v) < customspec.MinKeyLen {
			return nil, fmt.Errorf("pipeline.custom.spec_key_refs[%d]: chiave troppo corta (almeno %d byte)", i, customspec.MinKeyLen)
		}
		keys = append(keys, []byte(v))
	}
	return keys, nil
}

// NewCustomSteps crea il costruttore degli step personalizzati del worker.
func NewCustomSteps(cfg *config.Config, sp secrets.Provider, keys customspec.Keys, rec audit.Recorder, gov *governance.Policy, log *slog.Logger) *customstep.Builder {
	c := cfg.Pipeline.Custom
	return &customstep.Builder{
		Secrets:    sp,
		Keys:       keys,
		Revoked:    c.RevokedSpecs,
		Dialer:     &customstep.SSHDialer{KnownHostsPath: c.KnownHosts, InsecureIgnoreHostKey: c.InsecureIgnoreHostKey},
		HTTP:       customstep.NewHTTPClient(),
		AllowHTTP:  c.AllowHTTP,
		SecretsDir: c.SecretsDir,
		Audit:      rec,
		Frozen: func(application, environment string, at time.Time) bool {
			return gov.Frozen(application, environment, at) != nil
		},
		Logger: log.With("component", "step"),
	}
}
