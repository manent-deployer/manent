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
	// ("25%") of the pass targets. Default 1; a wave can set its own.
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
	// MaxUnavailable, if set, replaces the rollout's for the passes of this wave.
	MaxUnavailable string `json:"maxUnavailable,omitempty" yaml:"maxUnavailable,omitempty"`
	// BatchBake, if set, adds a gate after every batch of a pass but the last (the stage gate
	// follows it): the probes read the batch targets for this time ("0s": a single read).
	BatchBake string `json:"batchBake,omitempty" yaml:"batchBake,omitempty"`
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
	// Analysis, if set, judges the metrics with a weighted score instead of requiring every
	// one of them to be green (canary analysis in the style of Kayenta).
	Analysis *CanaryAnalysis `json:"analysis,omitempty" yaml:"analysis,omitempty"`
}

// CanaryAnalysis are the score thresholds of the canary analysis (0-100): during the bake a
// score below Marginal stops the stage, at the end the score must reach Pass.
type CanaryAnalysis struct {
	Pass     float64 `json:"pass,omitempty" yaml:"pass,omitempty"`
	Marginal float64 `json:"marginal,omitempty" yaml:"marginal,omitempty"`
}

// Default score thresholds of the canary analysis.
const (
	DefaultAnalysisPass     = 95
	DefaultAnalysisMarginal = 75
)

// Thresholds returns pass and marginal with their defaults.
func (a *CanaryAnalysis) Thresholds() (pass, marginal float64) {
	pass, marginal = DefaultAnalysisPass, DefaultAnalysisMarginal
	if a != nil && a.Pass > 0 {
		pass = a.Pass
	}
	if a != nil && a.Marginal > 0 {
		marginal = a.Marginal
	}
	return pass, min(marginal, pass)
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
	// Provider is the metrics system: prometheus (default), datadog, cloudwatch, newrelic or
	// elastic, configured in manent.yml.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Analysis is threshold (default: the value or the ratio against the stable version) or
	// mann-whitney: the series of candidate and stable are compared with the Mann-Whitney U test.
	Analysis string `json:"analysis,omitempty" yaml:"analysis,omitempty"`
	// Direction is what counts as worse with mann-whitney: increase (default), decrease or either.
	Direction string `json:"direction,omitempty" yaml:"direction,omitempty"`
	// Alpha is the significance level of the test (default 0.05).
	Alpha float64 `json:"alpha,omitempty" yaml:"alpha,omitempty"`
	// MinRatio is the minimum candidate/stable ratio with direction decrease (default 1); with
	// mann-whitney MaxRatio defaults to 1 too: any significant worsening fails.
	MinRatio *float64 `json:"minRatio,omitempty" yaml:"minRatio,omitempty"`
	// Lookback is the window of the series (default: the time elapsed in the stage).
	Lookback string `json:"lookback,omitempty" yaml:"lookback,omitempty"`
	// Step is the resolution of the series (default 15s).
	Step string `json:"step,omitempty" yaml:"step,omitempty"`
	// MinPoints are the points of each series needed for a judgement (default 5): with fewer
	// the metric is inconclusive, and red at the end of the bake.
	MinPoints int `json:"minPoints,omitempty" yaml:"minPoints,omitempty"`
	// Weight of the metric in the analysis score (default 1).
	Weight *float64 `json:"weight,omitempty" yaml:"weight,omitempty"`
	// Critical makes the stage fail when the metric fails, whatever the score.
	Critical bool `json:"critical,omitempty" yaml:"critical,omitempty"`
}

// Metric providers.
const (
	ProviderPrometheus = "prometheus"
	ProviderDatadog    = "datadog"
	ProviderCloudWatch = "cloudwatch"
	ProviderNewRelic   = "newrelic"
	ProviderElastic    = "elastic"
)

// MetricProviders are the supported metrics systems.
var MetricProviders = []string{ProviderPrometheus, ProviderDatadog, ProviderCloudWatch, ProviderNewRelic, ProviderElastic}

// Metric analyses and directions.
const (
	AnalysisThreshold   = "threshold"
	AnalysisMannWhitney = "mann-whitney"
	DirectionIncrease   = "increase"
	DirectionDecrease   = "decrease"
	DirectionEither     = "either"
)

// Defaults of the statistical analysis.
const (
	DefaultAlpha     = 0.05
	DefaultStep      = 15 * time.Second
	DefaultMinPoints = 5
)

// ProviderOrDefault returns the metrics system, prometheus by default.
func (m MetricGate) ProviderOrDefault() string {
	if m.Provider == "" {
		return ProviderPrometheus
	}
	return m.Provider
}

// WeightOrDefault returns the weight in the analysis score.
func (m MetricGate) WeightOrDefault() float64 {
	if m.Weight == nil {
		return 1
	}
	return *m.Weight
}

// StepDuration returns the resolution of the series.
func (m MetricGate) StepDuration() time.Duration {
	if d, err := time.ParseDuration(m.Step); err == nil && d > 0 {
		return d
	}
	return DefaultStep
}

// LookbackDuration returns the window of the series; zero means the time elapsed in the stage.
func (m MetricGate) LookbackDuration() time.Duration {
	d, _ := time.ParseDuration(m.Lookback)
	return d
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

// BatchGate returns the observation time of the batch gate and whether the gate is enabled.
func (w Wave) BatchGate() (time.Duration, bool) {
	if w.BatchBake == "" {
		return 0, false
	}
	d, _ := time.ParseDuration(w.BatchBake)
	return d, true
}

// IntervalDuration returns the check frequency.
func (h *Health) IntervalDuration() time.Duration {
	if h == nil || h.Interval == "" {
		return DefaultHealthInterval
	}
	d, _ := time.ParseDuration(h.Interval)
	return d
}

// MaxUnavailableFor returns the targets of wave w updated together out of n targets (at least
// 1): the wave's maxUnavailable, otherwise the rollout's.
func (r *Rollout) MaxUnavailableFor(w Wave, n int) int {
	v := strings.TrimSpace(w.MaxUnavailable)
	if v == "" {
		v = strings.TrimSpace(r.MaxUnavailable)
	}
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
		if w.BatchBake != "" {
			if d, err := time.ParseDuration(w.BatchBake); err != nil || d < 0 || d > maxBake {
				errs = append(errs, fmt.Errorf("%s.batchBake %q non valido (durata tra 0 e 24h, es. 1m)", p, w.BatchBake))
			}
			if r.IsTraffic() {
				errs = append(errs, fmt.Errorf("%s.batchBake non ammesso con strategy: traffic (il passo è un unico lotto)", p))
			}
		}
		if w.MaxUnavailable != "" {
			errs = append(errs, validMaxUnavailable(p+".maxUnavailable", w.MaxUnavailable))
			if r.IsTraffic() {
				errs = append(errs, fmt.Errorf("%s.maxUnavailable non ammesso con strategy: traffic (il passo è un unico lotto)", p))
			}
		}
	}
	errs = append(errs, validMaxUnavailable("rollout.maxUnavailable", r.MaxUnavailable))
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

// validMaxUnavailable checks a maxUnavailable value: a number >= 1 or a percentage 1-100%.
func validMaxUnavailable(field, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if err != nil || n < 1 || (strings.HasSuffix(v, "%") && n > 100) {
		return fmt.Errorf("%s %q non valido (numero >= 1 o percentuale 1-100%%)", field, v)
	}
	return nil
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
			if !validName(m.Name) || !strings.Contains(m.Query, "{track}") || (m.MaxRatio == nil && m.MaxDelta == nil && m.Analysis != AnalysisMannWhitney) {
				errs = append(errs, fmt.Errorf("rollout.health.metrics[%d]: con compare: stable servono name, una query con {track} e maxRatio o maxDelta (o analysis: mann-whitney)", i))
			}
		default:
			errs = append(errs, fmt.Errorf("rollout.health.metrics[%d].compare %q non valido (stable)", i, m.Compare))
		}
		if m.MinSamples < 0 || (m.MinSamples > 0 && strings.TrimSpace(m.SamplesQuery) == "") {
			errs = append(errs, fmt.Errorf("rollout.health.metrics[%d]: minSamples richiede samplesQuery", i))
		}
		errs = append(errs, m.validateAnalysis(i)...)
	}
	if a := h.Analysis; a != nil {
		if a.Pass < 0 || a.Pass > 100 || a.Marginal < 0 || a.Marginal > 100 || (a.Pass > 0 && a.Marginal > a.Pass) {
			errs = append(errs, errors.New("rollout.health.analysis: pass e marginal tra 0 e 100, marginal non oltre pass"))
		}
		if len(h.Metrics) == 0 {
			errs = append(errs, errors.New("rollout.health.analysis richiede almeno una metrica"))
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

// validateAnalysis checks provider, analysis and statistical parameters of a metric.
func (m MetricGate) validateAnalysis(i int) []error {
	var errs []error
	p := fmt.Sprintf("rollout.health.metrics[%d]", i)
	if m.Provider != "" && !slices.Contains(MetricProviders, m.Provider) {
		errs = append(errs, fmt.Errorf("%s.provider %q non valido (%s)", p, m.Provider, strings.Join(MetricProviders, ", ")))
	}
	switch m.Analysis {
	case "", AnalysisThreshold:
	case AnalysisMannWhitney:
		if m.Compare != CompareStable {
			errs = append(errs, fmt.Errorf("%s: analysis mann-whitney richiede compare: stable", p))
		}
	default:
		errs = append(errs, fmt.Errorf("%s.analysis %q non valido (threshold, mann-whitney)", p, m.Analysis))
	}
	if m.Direction != "" && m.Direction != DirectionIncrease && m.Direction != DirectionDecrease && m.Direction != DirectionEither {
		errs = append(errs, fmt.Errorf("%s.direction %q non valido (increase, decrease, either)", p, m.Direction))
	}
	if m.Alpha < 0 || m.Alpha > 0.5 {
		errs = append(errs, fmt.Errorf("%s.alpha deve essere tra 0 e 0.5", p))
	}
	for k, v := range map[string]string{"lookback": m.Lookback, "step": m.Step} {
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err != nil || d < time.Second {
			errs = append(errs, fmt.Errorf("%s.%s %q non valido (almeno 1s)", p, k, v))
		}
	}
	if m.MinPoints < 0 || (m.Weight != nil && *m.Weight < 0) {
		errs = append(errs, fmt.Errorf("%s: minPoints e weight non possono essere negativi", p))
	}
	return errs
}
