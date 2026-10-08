package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/joeg-ita/go-dart/internal/descriptor"
	"github.com/joeg-ita/go-dart/internal/pipeline"
	"github.com/joeg-ita/go-dart/internal/secrets"
	"github.com/joeg-ita/go-dart/pkg/plugin"
)

// Policy seleziona il plugin di deploy adatto al descrittore.
type Policy interface {
	Select(d *descriptor.Descriptor) (plugin.Deployer, error)
}

// DefaultPolicy usa il plugin indicato nel descrittore; altrimenti il primo plugin attivo
// (in ordine alfabetico) che supporta il tipo di artefatto.
type DefaultPolicy struct {
	Plugins *plugin.Registry
}

// Select applica la policy di selezione.
func (p DefaultPolicy) Select(d *descriptor.Descriptor) (plugin.Deployer, error) {
	if name := d.Deploy.Plugin; name != "" {
		dep, ok := p.Plugins.Get(name)
		if !ok {
			return nil, fmt.Errorf("plugin %q non attivo", name)
		}
		if !dep.Supports(d.Artifact.Type) {
			return nil, fmt.Errorf("plugin %q non supporta artefatti di tipo %q", name, d.Artifact.Type)
		}
		return dep, nil
	}
	for _, name := range p.Plugins.Active() {
		dep, _ := p.Plugins.Get(name)
		if dep.Supports(d.Artifact.Type) {
			return dep, nil
		}
	}
	return nil, fmt.Errorf("nessun plugin attivo supporta artefatti di tipo %q", d.Artifact.Type)
}

// Deploy seleziona il plugin tramite le policy e ne esegue il deploy.
type Deploy struct {
	// Policies associa il nome della policy (deploy.policy nel descrittore) alla sua implementazione.
	// La chiave "" è la policy di default.
	Policies map[string]Policy
	Secrets  secrets.Provider
	Logger   *slog.Logger
}

// Name restituisce il nome dello step.
func (Deploy) Name() string { return "deploy" }

// Run costruisce la richiesta per il plugin, la valida ed esegue il deploy.
func (s Deploy) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	d := in.Descriptor
	pol, ok := s.Policies[d.Deploy.Policy]
	if !ok {
		return nil, fmt.Errorf("policy di deploy %q sconosciuta", d.Deploy.Policy)
	}
	dep, err := pol.Select(d)
	if err != nil {
		return nil, err
	}
	req := &plugin.Request{
		DeploymentID: in.DeploymentID,
		Application:  d.Application,
		Environment:  d.Environment,
		Artifact: plugin.Artifact{
			Type:      d.Artifact.Type,
			Name:      d.Artifact.Name,
			Version:   d.Artifact.Version,
			LocalPath: in.Outputs["artifacts"][OutLocalPath],
			SHA256:    in.Outputs["artifacts"][OutSHA256],
		},
		Params:  d.Deploy.Params,
		Secrets: secrets.DescriptorResolver{Provider: s.Secrets, Descriptor: d},
	}
	for _, t := range d.Deploy.Targets {
		req.Targets = append(req.Targets, plugin.Target{Host: t.Host, Port: t.Port, User: t.User})
	}
	if err := dep.Validate(req); err != nil {
		return nil, fmt.Errorf("richiesta non valida per il plugin %q: %w", dep.Name(), err)
	}

	log := s.Logger.With("deployment", in.DeploymentID, "plugin", dep.Name())
	log.InfoContext(ctx, "deploy avviato", "targets", len(req.Targets))
	res, err := dep.Deploy(ctx, req)
	if err != nil {
		log.ErrorContext(ctx, "deploy fallito", "error", err)
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "deploy completato")
	return pipeline.Output{"plugin": dep.Name(), "result": string(raw)}, nil
}
