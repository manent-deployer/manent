package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/manent-deployer/manent/internal/audit"
	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/internal/customspec"
	"github.com/manent-deployer/manent/internal/customstep"
	"github.com/manent-deployer/manent/internal/policy"
	"github.com/manent-deployer/manent/internal/secrets"
)

// NewSpecKeys resolves the keys that sign custom step specs. Both the submitter (signature in
// the plan) and the worker (verification) use them, so both read them at startup.
func NewSpecKeys(cfg *config.Config, sp secrets.Provider) (customspec.Keys, error) {
	var keys customspec.Keys
	for i, ref := range cfg.Pipeline.Custom.SpecKeyRefs {
		v, err := sp.Resolve(context.Background(), ref)
		if err != nil {
			return nil, fmt.Errorf("pipeline.custom.spec_key_refs[%d]: %w", i, err)
		}
		if len(v) < customspec.MinKeyLen {
			return nil, fmt.Errorf("pipeline.custom.spec_key_refs[%d]: key too short (at least %d bytes)", i, customspec.MinKeyLen)
		}
		keys = append(keys, []byte(v))
	}
	return keys, nil
}

// NewCustomSteps creates the worker's custom step builder.
func NewCustomSteps(cfg *config.Config, sp secrets.Provider, keys customspec.Keys, rec audit.Recorder, gov *policy.Runtime, log *slog.Logger) *customstep.Builder {
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
