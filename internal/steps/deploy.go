package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/pkg/plugin"
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
	// Artifacts garantisce la presenza dell'artefatto anche se il deploy è stato recuperato su un altro worker.
	Artifacts ArtifactStore
	// Locators sono le strategie con cui individuare la release a cui tornare in caso di
	// rollback, per nome (rollback.locator).
	Locators map[string]plugin.ReleaseLocator
	Logger   *slog.Logger
}

// Name restituisce il nome dello step.
func (Deploy) Name() string { return "deploy" }

// Run costruisce la richiesta per il plugin, la valida ed esegue il deploy.
func (s Deploy) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	dep, req, log, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "deploy avviato", "targets", len(req.Targets))
	deploy := dep.Deploy
	if req.Traffic != nil {
		// Rilascio per percentuale: la nuova versione va accanto a quella stabile, senza traffico.
		sd, ok := dep.(plugin.SlotDeployer)
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("il plugin %q non sa rilasciare una versione affiancata", dep.Name()))
		}
		deploy = sd.DeployCandidate
	}
	res, err := deploy(ctx, req)
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

// DryRun seleziona il plugin e ne valida la richiesta come Run, ma invece di Deploy chiama
// il DryRun del plugin; se il plugin non lo implementa si ferma alla validazione.
func (s Deploy) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	dep, req, log, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	out := pipeline.Output{"plugin": dep.Name(), "dry_run": "validated"}
	dr, ok := dep.(plugin.DryRunner)
	if !ok {
		log.InfoContext(ctx, "dry-run: richiesta valida, il plugin non supporta verifiche sui target", "targets", len(req.Targets))
		return out, nil
	}
	log.InfoContext(ctx, "dry-run avviato", "targets", len(req.Targets))
	res, err := dr.DryRun(ctx, req)
	if errors.Is(err, plugin.ErrDryRunUnsupported) {
		log.InfoContext(ctx, "dry-run: richiesta valida, il plugin non supporta verifiche sui target", "targets", len(req.Targets))
		return out, nil
	}
	if err != nil {
		log.ErrorContext(ctx, "dry-run fallito", "error", err)
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "dry-run completato")
	out["dry_run"] = "checked"
	out["result"] = string(raw)
	return out, nil
}

// prepare seleziona il plugin, garantisce la presenza dell'artefatto e costruisce la richiesta validata.
func (s Deploy) prepare(ctx context.Context, in pipeline.Input) (plugin.Deployer, *plugin.Request, *slog.Logger, error) {
	dep, log, err := s.selectPlugin(in)
	if err != nil {
		return nil, nil, nil, err
	}
	art := in.Outputs["artifacts"]
	localPath, refetched, err := s.Artifacts.Ensure(ctx, in.DeploymentID, in.Descriptor.Artifact.Source, art[OutLocalPath], art[OutSHA256])
	if err != nil {
		return nil, nil, nil, err
	}
	if refetched {
		log.WarnContext(ctx, "artefatto assente su questo worker, riscaricato", "path", localPath)
	}
	req, err := s.request(dep, in, localPath)
	if err != nil {
		return nil, nil, nil, err
	}
	return dep, req, log, nil
}

// selectPlugin applica la policy di selezione del plugin.
func (s Deploy) selectPlugin(in pipeline.Input) (plugin.Deployer, *slog.Logger, error) {
	d := in.Descriptor
	pol, ok := s.Policies[d.Deploy.Policy]
	if !ok {
		return nil, nil, fmt.Errorf("policy di deploy %q sconosciuta", d.Deploy.Policy)
	}
	dep, err := pol.Select(d)
	if err != nil {
		return nil, nil, err
	}
	return dep, s.Logger.With("deployment", in.DeploymentID, "plugin", dep.Name()), nil
}

// request costruisce e valida la richiesta per il plugin.
func (s Deploy) request(dep plugin.Deployer, in pipeline.Input, localPath string) (*plugin.Request, error) {
	d := in.Descriptor
	art := in.Outputs["artifacts"]
	req := &plugin.Request{
		DeploymentID: in.DeploymentID,
		Application:  d.Application,
		Environment:  d.Environment,
		Artifact: plugin.Artifact{
			Type:      d.Artifact.Type,
			Name:      d.Artifact.Name,
			Version:   d.Artifact.Version,
			LocalPath: localPath,
			SHA256:    art[OutSHA256],
		},
		Params:  d.Deploy.Params,
		Secrets: secrets.DescriptorResolver{Provider: s.Secrets, Descriptor: d},
		Traffic: trafficRequest(in),
	}
	for _, t := range d.Deploy.Targets {
		req.Targets = append(req.Targets, plugin.Target{Host: t.Host, Port: t.Port, User: t.User})
	}
	if err := dep.Validate(req); err != nil {
		// Non è marcato permanente: un plugin esterno può fallire Validate perché il processo
		// non è raggiungibile, e quel caso va ritentato.
		return nil, fmt.Errorf("richiesta non valida per il plugin %q: %w", dep.Name(), err)
	}
	return req, nil
}

// Chiavi dell'output del checkpoint di deploy.
const (
	OutPreviousVersion = "previous_version"
	OutPrevious        = "previous"
	OutLocator         = "locator"
)

// Checkpoint verifica che il plugin supporti il rollback e registra, tramite il locator della
// policy, la release in esercizio prima del deploy. Non modifica i target.
func (s Deploy) Checkpoint(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if _, ok := in.Outputs[TrafficOutput]; ok {
		// Nei rilasci per percentuale il ritorno è uno spostamento di traffico, fatto dal motore.
		return pipeline.Output{"mode": "traffic"}, nil
	}
	dep, log, err := s.selectPlugin(in)
	if err != nil {
		return nil, err
	}
	ok, err := plugin.CanRollback(ctx, dep)
	if err != nil {
		return nil, fmt.Errorf("verifica del supporto al rollback del plugin %q: %w", dep.Name(), err)
	}
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("rollback attivo ma il plugin %q non lo supporta", dep.Name()))
	}
	name := in.Descriptor.Rollback.Locator
	loc, ok := s.Locators[name]
	if !ok {
		return nil, fmt.Errorf("locator di rollback %q sconosciuto", name)
	}
	// L'artefatto non serve per leggere la release in esercizio: basta il percorso registrato.
	req, err := s.request(dep, in, in.Outputs["artifacts"][OutLocalPath])
	if err != nil {
		return nil, err
	}
	rel, err := loc.Current(ctx, dep, req)
	if err != nil {
		return nil, fmt.Errorf("lettura della release in esercizio (locator %q): %w", name, err)
	}
	out := pipeline.Output{"plugin": dep.Name(), OutLocator: name, OutPreviousVersion: ""}
	if rel != nil {
		raw, err := json.Marshal(rel)
		if err != nil {
			return nil, err
		}
		out[OutPreviousVersion], out[OutPrevious] = rel.Version, string(raw)
	}
	log.InfoContext(ctx, "release in esercizio registrata per il rollback", "locator", name, "previous_version", out[OutPreviousVersion])
	return out, nil
}

// Compensate riporta i target alla release registrata dal checkpoint. Senza una release
// precedente (primo deploy) non c'è nulla da ripristinare.
func (s Deploy) Compensate(ctx context.Context, in pipeline.Input, cp pipeline.Output) (pipeline.Output, error) {
	if cp["mode"] == "traffic" {
		return pipeline.Output{pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "rilascio per percentuale: il traffico torna alla versione stabile"}, nil
	}
	dep, log, err := s.selectPlugin(in)
	if err != nil {
		return nil, err
	}
	if cp[OutPrevious] == "" {
		log.WarnContext(ctx, "rollback: nessuna release precedente da ripristinare")
		return pipeline.Output{"plugin": dep.Name(), pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "nessuna release precedente"}, nil
	}
	var prev plugin.Release
	if err := json.Unmarshal([]byte(cp[OutPrevious]), &prev); err != nil {
		return nil, fmt.Errorf("release precedente non leggibile: %w", err)
	}
	rb, ok := dep.(plugin.Rollbacker)
	if !ok {
		return nil, fmt.Errorf("il plugin %q non supporta il rollback", dep.Name())
	}
	req, err := s.request(dep, in, in.Outputs["artifacts"][OutLocalPath])
	if err != nil {
		return nil, err
	}
	log.WarnContext(ctx, "rollback avviato", "version", prev.Version, "targets", len(req.Targets))
	res, err := rb.Rollback(ctx, req, prev)
	if err != nil {
		log.ErrorContext(ctx, "rollback fallito", "version", prev.Version, "error", err)
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	log.WarnContext(ctx, "rollback completato", "version", prev.Version)
	return pipeline.Output{"plugin": dep.Name(), pipeline.OutRollback: pipeline.RollbackCompleted, "version": prev.Version, "result": string(raw)}, nil
}
