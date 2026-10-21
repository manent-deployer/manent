package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// attrExports is the workflow attribute with the exports of a successful deploy (JSON object):
// it is recorded together with the outcome, so whoever waits for the deploy (after) finds them
// as soon as it is settled.
const attrExports = "exports"

// errImportPending means that the imported deploy has not succeeded yet, or does not exist yet
// (deploy of a release not yet enqueued). It is temporary: at submission the check is left to
// the worker, where the dependency (after) has already succeeded.
var errImportPending = errors.New("the imported deploy has not succeeded yet")

// resolveFunc resolves the environment configuration of a descriptor.
type resolveFunc func(context.Context, descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error)

// withImports wraps the resolution of the environment configuration with the imports. Those of
// the descriptor are resolved first: their targets and parameters are descriptor additions,
// subject to the descriptor policy and to the merge rules. Those of the environment
// configuration are resolved after the merge, which brings them into the effective descriptor.
// The effective descriptor no longer has imports; the meta records the imported deploys
// (import.<name> = <deploy>@<version>). resolve can be nil (no environment configuration).
func (e *Engine) withImports(resolve resolveFunc) resolveFunc {
	return func(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
		imported := map[string]string{}
		d, err := e.applyImports(ctx, d, imported)
		if err != nil {
			return d, nil, err
		}
		eff, meta := d, map[string]string(nil)
		if resolve != nil {
			if eff, meta, err = resolve(ctx, d); err != nil {
				return eff, nil, err
			}
		}
		if eff, err = e.applyImports(ctx, eff, imported); err != nil {
			return eff, nil, err
		}
		if len(imported) == 0 {
			return eff, meta, nil
		}
		if len(eff.Deploy.Targets) == 0 {
			return eff, nil, pipeline.Permanent(errors.New("no deploy targets after the imports: the imported exports gave none"))
		}
		if meta == nil {
			meta = map[string]string{}
		}
		maps.Copy(meta, imported)
		meta["targets"] = strconv.Itoa(len(eff.Deploy.Targets))
		return eff, meta, nil
	}
}

// importResolve returns the resolution with imports of the given resolver (nil: none).
func (e *Engine) importResolve(r ConfigResolver) resolveFunc {
	if r == nil {
		return e.withImports(nil)
	}
	return e.withImports(r.Resolve)
}

// applyImports reads the exports of the imports and replaces deploy.targetsFrom and
// deploy.paramsFrom with targets and parameters. imported receives the deploys read.
func (e *Engine) applyImports(ctx context.Context, d descriptor.Descriptor, imported map[string]string) (descriptor.Descriptor, error) {
	if !d.HasImports() {
		return d, nil
	}
	exports := map[string]map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(d.Imports)) {
		src, ex, err := e.readImport(ctx, d, d.Imports[name])
		if err != nil {
			return d, fmt.Errorf("imports.%s: %w", name, err)
		}
		exports[name], imported["import."+name] = ex, src
	}
	value := func(r descriptor.ExportRef) (string, error) {
		v, ok := exports[r.Import][r.Export]
		if !ok {
			return "", pipeline.Permanent(fmt.Errorf("imports.%s (%s): no export %q", r.Import, imported["import."+r.Import], r.Export))
		}
		return v, nil
	}
	targets := slices.Clone(d.Deploy.Targets)
	for _, r := range d.Deploy.TargetsFrom {
		v, err := value(r)
		if err != nil {
			return d, err
		}
		ts, err := descriptor.ParseTargets(v)
		if err != nil {
			return d, pipeline.Permanent(fmt.Errorf("targetsFrom %s.%s: %w", r.Import, r.Export, err))
		}
		for _, t := range ts {
			if !slices.ContainsFunc(targets, func(o descriptor.Target) bool { return o.Host == t.Host && o.Port == t.Port }) {
				targets = append(targets, t)
			}
		}
	}
	params := maps.Clone(d.Deploy.Params)
	for _, k := range slices.Sorted(maps.Keys(d.Deploy.ParamsFrom)) {
		v, err := value(d.Deploy.ParamsFrom[k])
		if err != nil {
			return d, err
		}
		if old, ok := params[k]; ok && old != v {
			return d, pipeline.Permanent(fmt.Errorf("paramsFrom.%s: the parameter is already set to a different value", k))
		}
		if params == nil {
			params = map[string]string{}
		}
		params[k] = v
	}
	d.Deploy.Targets, d.Deploy.Params = targets, params
	d.Imports, d.Deploy.TargetsFrom, d.Deploy.ParamsFrom = nil, nil, nil
	return d, nil
}

// readImport reads the exports of an import and returns them with the deploy they come from
// ("<id>@<version>"). A deploy that does not exist yet or is still running is temporary
// (errImportPending); one ended without success, a dry-run or an environment without release
// in service are permanent.
func (e *Engine) readImport(_ context.Context, d descriptor.Descriptor, imp descriptor.Import) (string, map[string]string, error) {
	if id := imp.Deployment; id != "" {
		states, err := e.states([]string{id})
		if err != nil {
			return "", nil, err
		}
		s, ok := states[id]
		switch {
		case !ok:
			return "", nil, fmt.Errorf("%w: deploy %s does not exist yet", errImportPending, id)
		case s.DryRun:
			return "", nil, pipeline.Permanent(fmt.Errorf("deploy %s is a dry-run", id))
		case s.State == StateSuccess:
			return id + "@" + s.Version, s.Exports, nil
		case Terminal(s.State):
			return "", nil, pipeline.Permanent(fmt.Errorf("deploy %s ended in %s", id, s.State))
		default:
			return "", nil, fmt.Errorf("%w: deploy %s is %s", errImportPending, id, s.State)
		}
	}
	app, env, artifact := imp.Current.Application, cmp.Or(imp.Current.Environment, d.Environment), imp.Current.Artifact
	entries, err := e.inventory(InventoryFilter{Application: app, Environment: env, Artifact: artifact})
	if err != nil {
		return "", nil, err
	}
	var found []InventoryEntry
	for _, ie := range entries {
		if ie.Tenant == d.Tenant && ie.Current != nil {
			found = append(found, ie)
		}
	}
	switch {
	case len(found) == 1:
		return found[0].Current.ID + "@" + found[0].Current.Version, found[0].Current.Exports, nil
	case len(found) > 1:
		return "", nil, pipeline.Permanent(fmt.Errorf("%s/%s has %d artifacts in service: choose one with imports.<name>.current.artifact", app, env, len(found)))
	}
	where := app + "/" + env
	if artifact != "" {
		where += " (artifact " + artifact + ")"
	}
	if d.Tenant != "" {
		where += " (tenant " + d.Tenant + ")"
	}
	return "", nil, pipeline.Permanent(fmt.Errorf("no release in service of %s", where))
}

// exportsOf computes the declared exports from the step outputs of a successful deploy. In a
// rollout the same step runs once per pass and batch ("deploy@eu-1#0"): targets exports join
// the lists, string exports must have the same value everywhere.
func exportsOf(d *descriptor.Descriptor, steps []StepResult) (map[string]string, error) {
	if len(d.Exports) == 0 || d.DryRun {
		return nil, nil
	}
	out := map[string]string{}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(d.Exports)) {
		ex := d.Exports[name]
		step, key := ex.Source()
		var values []string
		for _, s := range steps {
			if s.Name != step && !strings.HasPrefix(s.Name, step+"@") {
				continue
			}
			v, ok := s.Output[pipeline.OutPluginPrefix+key]
			if !ok {
				v, ok = s.Output[key]
			}
			if ok {
				values = append(values, v)
			}
		}
		v, err := joinExport(ex, values)
		if err == nil {
			err = ex.Check(v)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("exports.%s (%s): %w", name, ex.From, err))
			continue
		}
		out[name] = v
	}
	total := 0
	for name, v := range out {
		total += len(name) + len(v)
	}
	if total > descriptor.MaxExportsBytes {
		errs = append(errs, fmt.Errorf("exports: %d bytes in all, at most %d", total, descriptor.MaxExportsBytes))
	}
	return out, errors.Join(errs...)
}

// joinExport combines the values of an export read from several runs of the step.
func joinExport(ex descriptor.Export, values []string) (string, error) {
	switch {
	case len(values) == 0:
		return "", errors.New("the step produced no such output")
	case len(values) == 1:
		return values[0], nil
	case !ex.IsTargets():
		for _, v := range values[1:] {
			if v != values[0] {
				return "", errors.New("the step runs produced different values")
			}
		}
		return values[0], nil
	}
	var all []descriptor.Target
	for _, v := range values {
		ts, err := descriptor.ParseTargets(v)
		if err != nil {
			return "", err
		}
		for _, t := range ts {
			if !slices.ContainsFunc(all, func(o descriptor.Target) bool { return o.Host == t.Host && o.Port == t.Port }) {
				all = append(all, t)
			}
		}
	}
	raw, err := json.Marshal(all)
	return string(raw), err
}

// exportsAttr returns the exports recorded in the workflow attributes.
func exportsAttr(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	var out map[string]string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// releaseImports replaces in the imports the names of the release deploys with their IDs.
func releaseImports(imports map[string]descriptor.Import, ids map[string]string) map[string]descriptor.Import {
	if len(imports) == 0 {
		return imports
	}
	out := make(map[string]descriptor.Import, len(imports))
	for name, imp := range imports {
		if id, ok := ids[imp.Deployment]; ok {
			imp.Deployment = id
		}
		out[name] = imp
	}
	return out
}
