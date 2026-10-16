package descriptor

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joeg-ita/manent/internal/labels"
)

// Behaviors of a rollout when a pass fails.
const (
	// OnFailureStop stops the rollout: passes already done stay on the new version.
	OnFailureStop = "stop"
	// OnFailureRollbackWave rolls back only the failed pass.
	OnFailureRollbackWave = "rollback-wave"
	// OnFailureRollbackAll rolls back all passes, in reverse order.
	OnFailureRollbackAll = "rollback-all"
)

// Ways to split a wave into passes.
const (
	ByNone   = ""
	ByAZ     = "az"
	ByRegion = "region"
)

// Rollout is the wave release strategy: targets are split into waves (label selectors),
// every wave into passes per AZ or region, and between passes there is an observation time
// with a health gate.
type Rollout struct {
	// Strategy is waves (default: waves of targets) or traffic (side-by-side version and traffic
	// shifted by percentages).
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	// Traffic are the steps of the traffic strategy.
	Traffic *TrafficRollout `json:"traffic,omitempty" yaml:"traffic,omitempty"`
	// Regions is the region order; unlisted ones follow in alphabetical order.
	Regions []string `json:"regions,omitempty" yaml:"regions,omitempty"`
	Waves   []Wave   `json:"waves,omitempty" yaml:"waves,omitempty"`
	// MaxUnavailable are the targets updated together in a pass: a number or a percentage
	// ("25%") of the pass targets. Default 1.
	MaxUnavailable string `json:"maxUnavailable,omitempty" yaml:"maxUnavailable,omitempty"`
	// OnFailure is stop (default), rollback-wave or rollback-all.
	OnFailure string `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	// ApprovalsBetween lists the waves after which the rollout waits for an approval; with the
	// traffic strategy, the steps ("0" after the tests at weight 0, "25" after the 25% step).
	ApprovalsBetween []string `json:"approvalsBetween,omitempty" yaml:"approvalsBetween,omitempty"`
	Health           *Health  `json:"health,omitempty" yaml:"health,omitempty"`
}

// Wave is a rollout wave.
type Wave struct {
	Name string `json:"name" yaml:"name"`
	// Select chooses the wave targets (e.g. "region=eu-1, az=eu-1a").
	Select string `json:"select" yaml:"select"`
	// By splits the wave into passes: "az", "region" or no split.
	By string `json:"by,omitempty" yaml:"by,omitempty"`
	// ParallelRegions are the regions updated together in the same pass (default 1).
	ParallelRegions int `json:"parallelRegions,omitempty" yaml:"parallelRegions,omitempty"`
	// Bake is the observation time after every pass (e.g. "10m").
	Bake string `json:"bake,omitempty" yaml:"bake,omitempty"`
}

// Health are the checks run during and at the end of the observation time.
type Health struct {
	Probes  []Probe      `json:"probes,omitempty" yaml:"probes,omitempty"`
	Metrics []MetricGate `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Alerts  *AlertGate   `json:"alerts,omitempty" yaml:"alerts,omitempty"`
	// Interval is the frequency of the checks during the bake (default 30s).
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`
	// FailuresToStop are the consecutive red checks that stop the pass (default 1).
	FailuresToStop int `json:"failuresToStop,omitempty" yaml:"failuresToStop,omitempty"`
}

// Probe is an HTTP probe to every target of the pass. URL must contain {host}: probes reach
// only the targets, not arbitrary addresses.
type Probe struct {
	URL          string `json:"url" yaml:"url"`
	ExpectStatus int    `json:"expectStatus,omitempty" yaml:"expectStatus,omitempty"`
}

// MetricGate is a PromQL query evaluated for every pass; {application}, {environment},
// {region} and {az} are replaced, and with the traffic strategy {track} (slot) and {version}.
// The result must lie between Min and Max; with Compare "stable" the query is evaluated for
// the candidate and the stable version and the two values are compared.
type MetricGate struct {
	Name  string   `json:"name" yaml:"name"`
	Query string   `json:"query" yaml:"query"`
	Max   *float64 `json:"max,omitempty" yaml:"max,omitempty"`
	Min   *float64 `json:"min,omitempty" yaml:"min,omitempty"`
	// Compare is "stable" to compare the candidate version with the stable one.
	Compare string `json:"compare,omitempty" yaml:"compare,omitempty"`
	// MaxRatio is the maximum candidate/stable ratio (e.g. 1.5).
	MaxRatio *float64 `json:"maxRatio,omitempty" yaml:"maxRatio,omitempty"`
	// MaxDelta is the maximum candidate - stable difference.
	MaxDelta *float64 `json:"maxDelta,omitempty" yaml:"maxDelta,omitempty"`
	// SamplesQuery counts the observed requests; below MinSamples the check is not green.
	SamplesQuery string  `json:"samplesQuery,omitempty" yaml:"samplesQuery,omitempty"`
	MinSamples   float64 `json:"minSamples,omitempty" yaml:"minSamples,omitempty"`
}

// CompareStable compares the candidate version with the stable one.
const CompareStable = "stable"

// AlertGate blocks the pass if Alertmanager has an active alert that meets all the BlockOn
// conditions (e.g. "severity=critical") and concerns the application, the region or the AZ.
type AlertGate struct {
	BlockOn []string `json:"blockOn" yaml:"blockOn"`
}

// Default durations.
const (
	DefaultHealthInterval = 30 * time.Second
	maxBake               = 24 * time.Hour
)

// BakeDuration returns the wave's observation time.
func (w Wave) BakeDuration() time.Duration {
	d, _ := time.ParseDuration(w.Bake)
	return d
}

// IntervalDuration returns the check frequency.
func (h *Health) IntervalDuration() time.Duration {
	if h == nil || h.Interval == "" {
		return DefaultHealthInterval
	}
	d, _ := time.ParseDuration(h.Interval)
	return d
}

// MaxUnavailableFor returns the targets updated together out of n targets (at least 1).
func (r *Rollout) MaxUnavailableFor(n int) int {
	v := strings.TrimSpace(r.MaxUnavailable)
	if v == "" {
		return 1
	}
	if p, ok := strings.CutSuffix(v, "%"); ok {
		pct, _ := strconv.Atoi(p)
		return max(1, n*pct/100)
	}
	k, _ := strconv.Atoi(v)
	return max(1, k)
}

// OnFailureOrDefault returns the behavior on a failed pass.
func (r *Rollout) OnFailureOrDefault() string {
	if r.OnFailure == "" {
		if r.IsTraffic() {
			// Going back to the stable version is a traffic shift: it is the default.
			return OnFailureRollbackAll
		}
		return OnFailureStop
	}
	return r.OnFailure
}

// Validate checks the rollout structure. The match with the targets is checked after merging
// with the environment configuration (internal/rollout).
func (r *Rollout) Validate() error {
	var errs []error
	switch r.Strategy {
	case "", StrategyWaves:
		if len(r.Waves) == 0 {
			errs = append(errs, errors.New("rollout.waves: almeno un'ondata"))
		}
		if r.Traffic != nil {
			errs = append(errs, errors.New("rollout.traffic richiede strategy: traffic"))
		}
	case StrategyTraffic:
		if r.Traffic == nil {
			errs = append(errs, errors.New("rollout.traffic obbligatorio con strategy: traffic"))
		} else {
			errs = append(errs, r.Traffic.validate())
		}
	default:
		errs = append(errs, fmt.Errorf("rollout.strategy %q non valida (waves|traffic)", r.Strategy))
	}
	names := map[string]bool{}
	for i, w := range r.Waves {
		p := fmt.Sprintf("rollout.waves[%d]", i)
		if !validName(w.Name) || names[w.Name] {
			errs = append(errs, fmt.Errorf("%s.name %q non valido o duplicato: %s", p, w.Name, nameRule))
		}
		names[w.Name] = true
		if _, err := labels.Parse(w.Select); err != nil {
			errs = append(errs, fmt.Errorf("%s.select: %w", p, err))
		}
		switch w.By {
		case ByNone, ByAZ, ByRegion:
		default:
			errs = append(errs, fmt.Errorf("%s.by %q non valido (az|region)", p, w.By))
		}
		if w.ParallelRegions < 0 {
			errs = append(errs, fmt.Errorf("%s.parallelRegions deve essere >= 0", p))
		}
		if w.Bake != "" {
			if d, err := time.ParseDuration(w.Bake); err != nil || d < 0 || d > maxBake {
				errs = append(errs, fmt.Errorf("%s.bake %q non valido (durata tra 0 e 24h, es. 10m)", p, w.Bake))
			}
		}
	}
	if v := strings.TrimSpace(r.MaxUnavailable); v != "" {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
		if err != nil || n < 1 || (strings.HasSuffix(v, "%") && n > 100) {
			errs = append(errs, fmt.Errorf("rollout.maxUnavailable %q non valido (numero >= 1 o percentuale 1-100%%)", v))
		}
	}
	switch r.OnFailure {
	case "", OnFailureStop, OnFailureRollbackWave, OnFailureRollbackAll:
	default:
		errs = append(errs, fmt.Errorf("rollout.onFailure %q non valido (stop|rollback-wave|rollback-all)", r.OnFailure))
	}
	for _, a := range r.ApprovalsBetween {
		if r.IsTraffic() && r.Traffic != nil && slices.Contains(r.Traffic.stageNames(), a) {
			continue
		}
		if !names[a] {
			errs = append(errs, fmt.Errorf("rollout.approvalsBetween: ondata %q inesistente", a))
		}
	}
	for i, reg := range r.Regions {
		if !validName(reg) {
			errs = append(errs, fmt.Errorf("rollout.regions[%d] %q non valida: %s", i, reg, nameRule))
		}
	}
	if r.Health != nil {
		errs = append(errs, r.Health.validate())
	}
	return errors.Join(errs...)
}

func (h *Health) validate() error {
	var errs []error
	if h.Interval != "" {
		if d, err := time.ParseDuration(h.Interval); err != nil || d < time.Second {
			errs = append(errs, fmt.Errorf("rollout.health.interval %q non valido (almeno 1s)", h.Interval))
		}
	}
	if h.FailuresToStop < 0 {
		errs = append(errs, errors.New("rollout.health.failuresToStop deve essere >= 0"))
	}
	for i, p := range h.Probes {
		u, err := url.Parse(strings.ReplaceAll(p.URL, "{host}", "target.invalid"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.Contains(p.URL, "://{host}") {
			errs = append(errs, fmt.Errorf("rollout.health.probes[%d].url deve essere http(s)://{host}...: le sonde raggiungono solo i target", i))
		}
		if p.ExpectStatus != 0 && (p.ExpectStatus < 100 || p.ExpectStatus > 599) {
			errs = append(errs, fmt.Errorf("rollout.health.probes[%d].expectStatus non valido", i))
		}
	}
	for i, m := range h.Metrics {
		switch m.Compare {
		case "":
			if !validName(m.Name) || strings.TrimSpace(m.Query) == "" || (m.Max == nil && m.Min == nil) {
				errs = append(errs, fmt.Errorf("rollout.health.metrics[%d]: name, query e almeno uno tra min e max obbligatori", i))
			}
		case CompareStable:
			if !validName(m.Name) || !strings.Contains(m.Query, "{track}") || (m.MaxRatio == nil && m.MaxDelta == nil) {
				errs = append(errs, fmt.Errorf("rollout.health.metrics[%d]: con compare: stable servono name, una query con {track} e maxRatio o maxDelta", i))
			}
		default:
			errs = append(errs, fmt.Errorf("rollout.health.metrics[%d].compare %q non valido (stable)", i, m.Compare))
		}
		if m.MinSamples < 0 || (m.MinSamples > 0 && strings.TrimSpace(m.SamplesQuery) == "") {
			errs = append(errs, fmt.Errorf("rollout.health.metrics[%d]: minSamples richiede samplesQuery", i))
		}
	}
	if a := h.Alerts; a != nil {
		if len(a.BlockOn) == 0 {
			errs = append(errs, errors.New("rollout.health.alerts.blockOn: almeno una condizione"))
		}
		for _, c := range a.BlockOn {
			if k, v, ok := strings.Cut(c, "="); !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
				errs = append(errs, fmt.Errorf("rollout.health.alerts.blockOn %q non valido (etichetta=valore)", c))
			}
		}
	}
	return errors.Join(errs...)
}
