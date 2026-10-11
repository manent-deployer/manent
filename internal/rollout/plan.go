// Package rollout calcola il piano di un rilascio a ondate e verifica la salute dei target tra
// un passo e l'altro. L'esecuzione è del worker (internal/engine).
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

// Plan è la sequenza degli stadi di un rollout. Gli stadi sono eseguiti uno dopo l'altro, con
// il tempo di osservazione e l'health gate dopo ciascuno.
type Plan struct {
	Stages []Stage `json:"stages"`
}

// Stage è un gruppo di passi eseguiti insieme (region diverse della stessa ondata).
type Stage struct {
	// Name identifica lo stadio, es. "eu-1/eu-1a" o "resto/eu-2a+us-1a".
	Name   string `json:"name"`
	Wave   string `json:"wave"`
	Passes []Pass `json:"passes"`
	// Bake è il tempo di osservazione dopo lo stadio.
	Bake time.Duration `json:"bake"`
	// LastOfWave indica l'ultimo stadio dell'ondata (dopo il quale può servire un'approvazione).
	LastOfWave bool `json:"last_of_wave,omitempty"`
}

// Pass è il lavoro su una region (e, con by: az, su un'AZ) dentro uno stadio.
type Pass struct {
	// Name è univoco nel rollout: <ondata>/<region>[/<az>].
	Name    string              `json:"name"`
	Wave    string              `json:"wave"`
	Region  string              `json:"region,omitempty"`
	AZ      string              `json:"az,omitempty"`
	Targets []descriptor.Target `json:"targets"`
	// Batches divide i target in gruppi aggiornati insieme (max_unavailable).
	Batches [][]descriptor.Target `json:"batches"`
}

// Hosts restituisce gli host del passo, con la porta se indicata.
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

// Regions restituisce le region dello stadio.
func (s Stage) Regions() []string {
	var out []string
	for _, p := range s.Passes {
		if !slices.Contains(out, p.Region) {
			out = append(out, p.Region)
		}
	}
	return out
}

// TotalPasses conta i passi del piano.
func (p Plan) TotalPasses() int {
	n := 0
	for _, s := range p.Stages {
		n += len(s.Passes)
	}
	return n
}

// Build calcola il piano. Ogni target deve appartenere a una sola ondata, ogni ondata deve
// selezionare almeno un target; con la divisione per region o AZ i target devono avere le
// etichette region e az. Il risultato dipende solo dagli argomenti: il worker lo ricalcola
// identico dopo una recovery.
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
				errs = append(errs, fmt.Errorf("rollout: il target %s è selezionato sia da %q sia da %q", t.Host, r.Waves[owner[ti]].Name, w.Name))
				continue
			}
			owner[ti] = wi
			waveTargets[wi] = append(waveTargets[wi], t)
		}
		if len(waveTargets[wi]) == 0 {
			errs = append(errs, fmt.Errorf("rollout: l'ondata %q (%s) non seleziona alcun target", w.Name, w.Select))
		}
	}
	for ti, o := range owner {
		if o < 0 {
			errs = append(errs, fmt.Errorf("rollout: il target %s non appartiene ad alcuna ondata", targets[ti].Host))
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

// waveStages divide un'ondata in stadi. Con by: az ogni region è divisa in AZ (in ordine
// alfabetico) e lo stadio k contiene l'AZ k di parallelRegions region; con by: region ogni
// stadio contiene parallelRegions region intere; senza divisione l'ondata è un solo stadio.
func waveStages(w descriptor.Wave, ts []descriptor.Target, r *descriptor.Rollout) ([]Stage, error) {
	bake := w.BakeDuration()
	if w.By == descriptor.ByNone {
		p := pass(w.Name, "", "", ts, r)
		return []Stage{{Name: w.Name, Wave: w.Name, Passes: []Pass{p}, Bake: bake}}, nil
	}
	byRegion := map[string][]descriptor.Target{}
	for _, t := range ts {
		reg, az := t.Labels[labels.Region], t.Labels[labels.AZ]
		if reg == "" || (w.By == descriptor.ByAZ && az == "") {
			return nil, fmt.Errorf("rollout: l'ondata %q divide per %s ma il target %s non ha le etichette region e az", w.Name, w.By, t.Host)
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
				st.Passes = append(st.Passes, pass(w.Name, reg, "", byRegion[reg], r))
			}
			stages = append(stages, st)
			continue
		}
		// by: az — le AZ di ogni region del gruppo, una per stadio.
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
				st.Passes = append(st.Passes, pass(w.Name, reg, az, sel, r))
				names = append(names, az)
			}
			st.Name = w.Name + "/" + strings.Join(names, "+")
			stages = append(stages, st)
		}
	}
	return stages, nil
}

// orderRegions mette prima le region nell'ordine di Rollout.Regions, poi le altre in ordine alfabetico.
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

func pass(wave, region, az string, ts []descriptor.Target, r *descriptor.Rollout) Pass {
	name := wave
	if region != "" {
		name += "/" + region
	}
	if az != "" {
		name += "/" + az
	}
	p := Pass{Name: name, Wave: wave, Region: region, AZ: az, Targets: ts}
	if region == "" && len(ts) > 0 {
		// Ondata senza divisione: la region è nota se tutti i target ne hanno una sola.
		p.Region = common(ts, labels.Region)
		p.AZ = common(ts, labels.AZ)
	}
	size := r.MaxUnavailableFor(len(ts))
	for i := 0; i < len(ts); i += size {
		p.Batches = append(p.Batches, ts[i:min(i+size, len(ts))])
	}
	return p
}

// common restituisce il valore dell'etichetta se è lo stesso per tutti i target, altrimenti "".
func common(ts []descriptor.Target, key string) string {
	v := ts[0].Labels[key]
	for _, t := range ts[1:] {
		if t.Labels[key] != v {
			return ""
		}
	}
	return v
}
