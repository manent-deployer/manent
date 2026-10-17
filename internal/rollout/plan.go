// Package rollout computes the plan of a wave release and checks target health between
// passes. Execution belongs to the worker (internal/engine).
package rollout

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/labels"
)

// Plan is the sequence of a rollout's stages. Stages run one after the other, with the
// observation time and the health gate after each.
type Plan struct {
	Stages []Stage `json:"stages"`
}

// Stage is a group of passes run together (different regions of the same wave).
type Stage struct {
	// Name identifies the stage, e.g. "eu-1/eu-1a" or "rest/eu-2a+us-1a".
	Name   string `json:"name"`
	Wave   string `json:"wave"`
	Passes []Pass `json:"passes"`
	// Bake is the observation time after the stage.
	Bake time.Duration `json:"bake"`
	// LastOfWave marks the last stage of the wave (after which an approval may be needed).
	LastOfWave bool `json:"last_of_wave,omitempty"`
}

// Pass is the work on a region (and, with by: az, on an AZ) within a stage.
type Pass struct {
	// Name is unique in the rollout: <wave>/<region>[/<az>].
	Name    string              `json:"name"`
	Wave    string              `json:"wave"`
	Region  string              `json:"region,omitempty"`
	AZ      string              `json:"az,omitempty"`
	Targets []descriptor.Target `json:"targets"`
	// Batches splits the targets into groups updated together (max_unavailable).
	Batches [][]descriptor.Target `json:"batches"`
	// BatchGate enables the gate after every batch but the last, observed for BatchBake.
	BatchGate bool          `json:"batch_gate,omitempty"`
	BatchBake time.Duration `json:"batch_bake,omitempty"`
}

// Hosts returns the hosts of the pass, with the port if given.
func (p Pass) Hosts() []string {
	out := make([]string, len(p.Targets))
	for i, t := range p.Targets {
		out[i] = t.Host
		if t.Port != 0 {
			out[i] += ":" + strconv.Itoa(t.Port)
		}
	}
	return out
}

// Regions returns the regions of the stage.
func (s Stage) Regions() []string {
	var out []string
	for _, p := range s.Passes {
		if !slices.Contains(out, p.Region) {
			out = append(out, p.Region)
		}
	}
	return out
}

// TotalPasses counts the passes of the plan.
func (p Plan) TotalPasses() int {
	n := 0
	for _, s := range p.Stages {
		n += len(s.Passes)
	}
	return n
}

// Build computes the plan. Every target must belong to exactly one wave, every wave must
// select at least one target; with a split by region or AZ the targets must have the region
// and az labels. The result depends only on the arguments: the worker recomputes it
// identically after a recovery.
func Build(targets []descriptor.Target, r *descriptor.Rollout) (Plan, error) {
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	var errs []error
	owner := make([]int, len(targets))
	for i := range owner {
		owner[i] = -1
	}
	waveTargets := make([][]descriptor.Target, len(r.Waves))
	for wi, w := range r.Waves {
		sel, _ := labels.Parse(w.Select)
		for ti, t := range targets {
			if !sel.Matches(t.Labels) {
				continue
			}
			if owner[ti] >= 0 {
				errs = append(errs, fmt.Errorf("rollout: target %s is selected both by %q and by %q", t.Host, r.Waves[owner[ti]].Name, w.Name))
				continue
			}
			owner[ti] = wi
			waveTargets[wi] = append(waveTargets[wi], t)
		}
		if len(waveTargets[wi]) == 0 {
			errs = append(errs, fmt.Errorf("rollout: wave %q (%s) selects no target", w.Name, w.Select))
		}
	}
	for ti, o := range owner {
		if o < 0 {
			errs = append(errs, fmt.Errorf("rollout: target %s belongs to no wave", targets[ti].Host))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Plan{}, err
	}
	var plan Plan
	for wi, w := range r.Waves {
		stages, err := waveStages(w, waveTargets[wi], r)
		if err != nil {
			return Plan{}, err
		}
		stages[len(stages)-1].LastOfWave = true
		plan.Stages = append(plan.Stages, stages...)
	}
	return plan, nil
}

// waveStages splits a wave into stages. With by: az every region is split into AZs (in
// alphabetical order) and stage k holds AZ k of parallelRegions regions; with by: region every
// stage holds parallelRegions whole regions; without a split the wave is a single stage.
func waveStages(w descriptor.Wave, ts []descriptor.Target, r *descriptor.Rollout) ([]Stage, error) {
	bake := w.BakeDuration()
	if w.By == descriptor.ByNone {
		p := pass(w, "", "", ts, r)
		return []Stage{{Name: w.Name, Wave: w.Name, Passes: []Pass{p}, Bake: bake}}, nil
	}
	byRegion := map[string][]descriptor.Target{}
	for _, t := range ts {
		reg, az := t.Labels[labels.Region], t.Labels[labels.AZ]
		if reg == "" || (w.By == descriptor.ByAZ && az == "") {
			return nil, fmt.Errorf("rollout: wave %q splits by %s but target %s does not have the region and az labels", w.Name, w.By, t.Host)
		}
		byRegion[reg] = append(byRegion[reg], t)
	}
	regions := orderRegions(slices.Collect(maps.Keys(byRegion)), r.Regions)
	par := max(1, w.ParallelRegions)
	var stages []Stage
	for g := 0; g < len(regions); g += par {
		group := regions[g:min(g+par, len(regions))]
		if w.By == descriptor.ByRegion {
			st := Stage{Name: w.Name + "/" + strings.Join(group, "+"), Wave: w.Name, Bake: bake}
			for _, reg := range group {
				st.Passes = append(st.Passes, pass(w, reg, "", byRegion[reg], r))
			}
			stages = append(stages, st)
			continue
		}
		// by: az — the AZs of every region of the group, one per stage.
		azs := map[string][]string{}
		depth := 0
		for _, reg := range group {
			seen := map[string]bool{}
			for _, t := range byRegion[reg] {
				if az := t.Labels[labels.AZ]; !seen[az] {
					seen[az] = true
					azs[reg] = append(azs[reg], az)
				}
			}
			slices.Sort(azs[reg])
			depth = max(depth, len(azs[reg]))
		}
		for k := range depth {
			st := Stage{Wave: w.Name, Bake: bake}
			var names []string
			for _, reg := range group {
				if k >= len(azs[reg]) {
					continue
				}
				az := azs[reg][k]
				var sel []descriptor.Target
				for _, t := range byRegion[reg] {
					if t.Labels[labels.AZ] == az {
						sel = append(sel, t)
					}
				}
				st.Passes = append(st.Passes, pass(w, reg, az, sel, r))
				names = append(names, az)
			}
			st.Name = w.Name + "/" + strings.Join(names, "+")
			stages = append(stages, st)
		}
	}
	return stages, nil
}

// orderRegions puts first the regions in Rollout.Regions order, then the others alphabetically.
func orderRegions(found, order []string) []string {
	slices.Sort(found)
	out := make([]string, 0, len(found))
	for _, r := range order {
		if slices.Contains(found, r) {
			out = append(out, r)
		}
	}
	for _, r := range found {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

func pass(w descriptor.Wave, region, az string, ts []descriptor.Target, r *descriptor.Rollout) Pass {
	wave, name := w.Name, w.Name
	if region != "" {
		name += "/" + region
	}
	if az != "" {
		name += "/" + az
	}
	p := Pass{Name: name, Wave: wave, Region: region, AZ: az, Targets: ts}
	if region == "" && len(ts) > 0 {
		// Wave without a split: the region is known if all targets share a single one.
		p.Region = common(ts, labels.Region)
		p.AZ = common(ts, labels.AZ)
	}
	size := r.MaxUnavailableFor(w, len(ts))
	for i := 0; i < len(ts); i += size {
		p.Batches = append(p.Batches, ts[i:min(i+size, len(ts))])
	}
	p.BatchBake, p.BatchGate = w.BatchGate()
	return p
}

// common returns the label value if it is the same for all targets, otherwise "".
func common(ts []descriptor.Target, key string) string {
	v := ts[0].Labels[key]
	for _, t := range ts[1:] {
		if t.Labels[key] != v {
			return ""
		}
	}
	return v
}
