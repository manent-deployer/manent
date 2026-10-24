package steps

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
	"github.com/joeg-ita/manent/internal/secrets"
	"github.com/joeg-ita/manent/internal/tenant"
	"github.com/joeg-ita/manent/pkg/plugin"
)

// Policy selects the deploy plugin suited to the descriptor.
type Policy interface {
	Select(d *descriptor.Descriptor) (plugin.Deployer, error)
}

// DefaultPolicy uses the plugin named in the descriptor; otherwise the first active plugin
// (in alphabetical order) that supports the artifact type.
type DefaultPolicy struct {
	Plugins *plugin.Registry
}

// Select applies the selection policy.
func (p DefaultPolicy) Select(d *descriptor.Descriptor) (plugin.Deployer, error) {
	if name := d.Deploy.Plugin; name != "" {
		dep, ok := p.Plugins.Get(name)
		if !ok {
			return nil, fmt.Errorf("plugin %q not active", name)
		}
		if !dep.Supports(d.Artifact.Type) {
			return nil, fmt.Errorf("plugin %q does not support artifacts of type %q", name, d.Artifact.Type)
		}
		return dep, nil
	}
	for _, name := range p.Plugins.Active() {
		dep, _ := p.Plugins.Get(name)
		if dep.Supports(d.Artifact.Type) {
			return dep, nil
		}
	}
	return nil, fmt.Errorf("no active plugin supports artifacts of type %q", d.Artifact.Type)
}

// Deploy selects the plugin through the policies and runs its deploy.
type Deploy struct {
	// Policies maps the policy name (deploy.policy in the descriptor) to its implementation.
	// The "" key is the default policy.
	Policies map[string]Policy
	Secrets  secrets.Provider
	// Artifacts ensures the artifact is present even if the deploy was recovered on another worker.
	Artifacts ArtifactStore
	// Locators are the strategies to find the release to go back to on rollback, by name
	// (rollback.locator).
	Locators map[string]plugin.ReleaseLocator
	Logger   *slog.Logger
}

// Name returns the step name.
func (Deploy) Name() string { return "deploy" }

// Run builds the request for the plugin, validates it and runs the deploy.
func (s Deploy) Run(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	dep, req, log, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "deploy started", "targets", len(req.Targets))
	deploy := dep.Deploy
	if req.Traffic != nil {
		// Percentage rollout: the new version goes next to the stable one, without traffic.
		sd, ok := dep.(plugin.SlotDeployer)
		if !ok {
			return nil, pipeline.Permanent(fmt.Errorf("plugin %q cannot release a side-by-side version", dep.Name()))
		}
		deploy = sd.DeployCandidate
	}
	res, err := deploy(ctx, req)
	if err != nil {
		log.ErrorContext(ctx, "deploy failed", "error", err)
		return nil, unsupported(dep, err)
	}
	out, err := deployOutput(dep.Name(), res)
	if err != nil {
		log.ErrorContext(ctx, "invalid plugin outputs", "error", err)
		return nil, err
	}
	log.InfoContext(ctx, "deploy completed", "outputs", len(res.Outputs))
	return out, nil
}

// outputKeyRe limits the keys of the plugin outputs.
var outputKeyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// deployOutput is the output of the deploy step: plugin, result and the plugin outputs with
// the out. prefix. Invalid outputs are a plugin bug: a retry would give the same ones.
func deployOutput(name string, res *plugin.Result) (pipeline.Output, error) {
	r := plugin.Result{}
	if res != nil {
		r = *res
	}
	outputs := r.Outputs
	if len(outputs) > plugin.MaxOutputs {
		return nil, pipeline.Permanent(fmt.Errorf("plugin %q: at most %d outputs", name, plugin.MaxOutputs))
	}
	out := pipeline.Output{"plugin": name}
	for _, k := range slices.Sorted(maps.Keys(outputs)) {
		v := outputs[k]
		if !outputKeyRe.MatchString(k) || len(v) > plugin.MaxOutputBytes || strings.ContainsRune(v, 0) {
			return nil, pipeline.Permanent(fmt.Errorf("plugin %q: invalid output %q (key: letters, digits, '_', '-', '.', max 64; value max %d bytes)", name, k, plugin.MaxOutputBytes))
		}
		out[pipeline.OutPluginPrefix+k] = v
	}
	// The outputs are already in the step output: the result does not repeat them.
	r.Outputs = nil
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	out["result"] = string(raw)
	return out, nil
}

// DryRun selects the plugin and validates its request as Run does, but calls the plugin's
// DryRun instead of Deploy; if the plugin does not implement it, it stops at validation.
func (s Deploy) DryRun(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	dep, req, log, err := s.prepare(ctx, in)
	if err != nil {
		return nil, err
	}
	out := pipeline.Output{"plugin": dep.Name(), "dry_run": "validated"}
	dr, ok := dep.(plugin.DryRunner)
	if !ok {
		log.InfoContext(ctx, "dry-run: valid request, the plugin does not support checks on the targets", "targets", len(req.Targets))
		return out, nil
	}
	log.InfoContext(ctx, "dry-run started", "targets", len(req.Targets))
	res, err := dr.DryRun(ctx, req)
	if errors.Is(err, plugin.ErrDryRunUnsupported) {
		log.InfoContext(ctx, "dry-run: valid request, the plugin does not support checks on the targets", "targets", len(req.Targets))
		return out, nil
	}
	if err != nil {
		log.ErrorContext(ctx, "dry-run failed", "error", err)
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "dry-run completed")
	out["dry_run"] = "checked"
	out["result"] = string(raw)
	return out, nil
}

// prepare selects the plugin, ensures the artifact is present and builds the validated request.
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
		log.WarnContext(ctx, "artifact missing on this worker, downloaded again", "path", localPath)
	}
	req, err := s.request(dep, in, localPath)
	if err != nil {
		return nil, nil, nil, err
	}
	return dep, req, log, nil
}

// selectPlugin applies the plugin selection policy.
func (s Deploy) selectPlugin(in pipeline.Input) (plugin.Deployer, *slog.Logger, error) {
	d := in.Descriptor
	pol, ok := s.Policies[d.Deploy.Policy]
	if !ok {
		return nil, nil, fmt.Errorf("unknown deploy policy %q", d.Deploy.Policy)
	}
	dep, err := pol.Select(d)
	if err != nil {
		return nil, nil, err
	}
	return dep, s.Logger.With("deployment", in.DeploymentID, "plugin", dep.Name()), nil
}

// request builds and validates the request for the plugin.
func (s Deploy) request(dep plugin.Deployer, in pipeline.Input, localPath string) (*plugin.Request, error) {
	d := in.Descriptor
	art := in.Outputs["artifacts"]
	req := &plugin.Request{
		DeploymentID: in.DeploymentID,
		Tenant:       cmp.Or(d.Tenant, tenant.Untenanted),
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
		// Not marked permanent: an external plugin can fail Validate because the process is
		// unreachable, and that case must be retried.
		return nil, fmt.Errorf("invalid request for plugin %q: %w", dep.Name(), err)
	}
	return req, nil
}

// Output keys of the deploy checkpoint.
const (
	OutPreviousVersion = "previous_version"
	OutPrevious        = "previous"
	OutLocator         = "locator"
)

// Checkpoint checks that the plugin supports rollback and records, through the policy's
// locator, the release in service before the deploy. It does not change the targets.
func (s Deploy) Checkpoint(ctx context.Context, in pipeline.Input) (pipeline.Output, error) {
	if _, ok := in.Outputs[TrafficOutput]; ok {
		// In percentage rollouts the return is a traffic shift, done by the engine.
		return pipeline.Output{"mode": "traffic"}, nil
	}
	dep, log, err := s.selectPlugin(in)
	if err != nil {
		return nil, err
	}
	ok, err := plugin.CanRollback(ctx, dep)
	if err != nil {
		return nil, fmt.Errorf("checking rollback support of plugin %q: %w", dep.Name(), err)
	}
	if !ok {
		return nil, pipeline.Permanent(fmt.Errorf("rollback enabled but plugin %q does not support it", dep.Name()))
	}
	name := in.Descriptor.Rollback.Locator
	loc, ok := s.Locators[name]
	if !ok {
		return nil, fmt.Errorf("unknown rollback locator %q", name)
	}
	// The artifact is not needed to read the release in service: the recorded path is enough.
	req, err := s.request(dep, in, in.Outputs["artifacts"][OutLocalPath])
	if err != nil {
		return nil, err
	}
	rel, err := loc.Current(ctx, dep, req)
	if err != nil {
		return nil, fmt.Errorf("reading the release in service (locator %q): %w", name, err)
	}
	out := pipeline.Output{"plugin": dep.Name(), OutLocator: name, OutPreviousVersion: ""}
	if rel != nil {
		raw, err := json.Marshal(rel)
		if err != nil {
			return nil, err
		}
		out[OutPreviousVersion], out[OutPrevious] = rel.Version, string(raw)
	}
	log.InfoContext(ctx, "release in service recorded for the rollback", "locator", name, "previous_version", out[OutPreviousVersion])
	return out, nil
}

// Compensate brings the targets back to the release recorded by the checkpoint. Without a
// previous release (first deploy) there is nothing to restore.
func (s Deploy) Compensate(ctx context.Context, in pipeline.Input, cp pipeline.Output) (pipeline.Output, error) {
	if cp["mode"] == "traffic" {
		return pipeline.Output{pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "percentage rollout: the traffic goes back to the stable version"}, nil
	}
	dep, log, err := s.selectPlugin(in)
	if err != nil {
		return nil, err
	}
	if cp[OutPrevious] == "" {
		log.WarnContext(ctx, "rollback: no previous release to restore")
		return pipeline.Output{"plugin": dep.Name(), pipeline.OutRollback: pipeline.RollbackSkipped, "reason": "no previous release"}, nil
	}
	var prev plugin.Release
	if err := json.Unmarshal([]byte(cp[OutPrevious]), &prev); err != nil {
		return nil, fmt.Errorf("unreadable previous release: %w", err)
	}
	rb, ok := dep.(plugin.Rollbacker)
	if !ok {
		return nil, fmt.Errorf("plugin %q does not support rollback", dep.Name())
	}
	req, err := s.request(dep, in, in.Outputs["artifacts"][OutLocalPath])
	if err != nil {
		return nil, err
	}
	log.WarnContext(ctx, "rollback started", "version", prev.Version, "targets", len(req.Targets))
	res, err := rb.Rollback(ctx, req, prev)
	if err != nil {
		log.ErrorContext(ctx, "rollback failed", "version", prev.Version, "error", err)
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	log.WarnContext(ctx, "rollback completed", "version", prev.Version)
	return pipeline.Output{"plugin": dep.Name(), pipeline.OutRollback: pipeline.RollbackCompleted, "version": prev.Version, "result": string(raw)}, nil
}
