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

// Comportamenti di un rollout quando un passo fallisce.
const (
	// OnFailureStop ferma il rollout: i passi già superati restano sulla nuova versione.
	OnFailureStop = "stop"
	// OnFailureRollbackWave riporta indietro solo il passo fallito.
	OnFailureRollbackWave = "rollback-wave"
	// OnFailureRollbackAll riporta indietro tutti i passi, in ordine inverso.
	OnFailureRollbackAll = "rollback-all"
)

// Divisioni di un'ondata in passi.
const (
	ByNone   = ""
	ByAZ     = "az"
	ByRegion = "region"
)

// Rollout è la strategia di rilascio a ondate: i target sono divisi in ondate (selettori sulle
// etichette), ogni ondata in passi per AZ o region, e tra un passo e l'altro c'è un tempo di
// osservazione con un health gate.
type Rollout struct {
	// Strategy è waves (default: ondate di target) o traffic (versione affiancata e traffico
	// spostato per percentuali).
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	// Traffic sono i gradini della strategia traffic.
	Traffic *TrafficRollout `json:"traffic,omitempty" yaml:"traffic,omitempty"`
	// Regions è l'ordine delle region; quelle non elencate seguono in ordine alfabetico.
	Regions []string `json:"regions,omitempty" yaml:"regions,omitempty"`
	Waves   []Wave   `json:"waves,omitempty" yaml:"waves,omitempty"`
	// MaxUnavailable sono i target aggiornati insieme in un passo: un numero o una
	// percentuale ("25%") dei target del passo. Default 1.
	MaxUnavailable string `json:"maxUnavailable,omitempty" yaml:"maxUnavailable,omitempty"`
	// OnFailure è stop (default), rollback-wave o rollback-all.
	OnFailure string `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	// ApprovalsBetween elenca le ondate dopo le quali il rollout attende un'approvazione; con la
	// strategia traffic, i gradini ("0" dopo i test a peso 0, "25" dopo il gradino del 25%).
	ApprovalsBetween []string `json:"approvalsBetween,omitempty" yaml:"approvalsBetween,omitempty"`
	Health           *Health  `json:"health,omitempty" yaml:"health,omitempty"`
}

// Wave è un'ondata del rollout.
type Wave struct {
	Name string `json:"name" yaml:"name"`
	// Select sceglie i target dell'ondata (es. "region=eu-1, az=eu-1a").
	Select string `json:"select" yaml:"select"`
	// By divide l'ondata in passi: "az", "region" o nessuna divisione.
	By string `json:"by,omitempty" yaml:"by,omitempty"`
	// ParallelRegions sono le region aggiornate insieme nello stesso passo (default 1).
	ParallelRegions int `json:"parallelRegions,omitempty" yaml:"parallelRegions,omitempty"`
	// Bake è il tempo di osservazione dopo ogni passo (es. "10m").
	Bake string `json:"bake,omitempty" yaml:"bake,omitempty"`
}

// Health sono i controlli eseguiti durante e alla fine del tempo di osservazione.
type Health struct {
	Probes  []Probe      `json:"probes,omitempty" yaml:"probes,omitempty"`
	Metrics []MetricGate `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Alerts  *AlertGate   `json:"alerts,omitempty" yaml:"alerts,omitempty"`
	// Interval è la frequenza dei controlli durante il bake (default 30s).
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`
	// FailuresToStop sono i controlli rossi consecutivi che fermano il passo (default 1).
	FailuresToStop int `json:"failuresToStop,omitempty" yaml:"failuresToStop,omitempty"`
}

// Probe è una sonda HTTP verso ogni target del passo. URL deve contenere {host}: le sonde
// raggiungono solo i target, non indirizzi arbitrari.
type Probe struct {
	URL          string `json:"url" yaml:"url"`
	ExpectStatus int    `json:"expectStatus,omitempty" yaml:"expectStatus,omitempty"`
}

// MetricGate è una query PromQL valutata per ogni passo; {application}, {environment},
// {region} e {az} vengono sostituiti, e con la strategia traffic {track} (slot) e {version}.
// Il risultato deve stare tra Min e Max; con Compare "stable" la query è valutata per la
// versione candidata e per quella stabile e si confrontano i due valori.
type MetricGate struct {
	Name  string   `json:"name" yaml:"name"`
	Query string   `json:"query" yaml:"query"`
	Max   *float64 `json:"max,omitempty" yaml:"max,omitempty"`
	Min   *float64 `json:"min,omitempty" yaml:"min,omitempty"`
	// Compare è "stable" per confrontare la versione candidata con quella stabile.
	Compare string `json:"compare,omitempty" yaml:"compare,omitempty"`
	// MaxRatio è il rapporto massimo candidata/stabile (es. 1.5).
	MaxRatio *float64 `json:"maxRatio,omitempty" yaml:"maxRatio,omitempty"`
	// MaxDelta è la differenza massima candidata - stabile.
	MaxDelta *float64 `json:"maxDelta,omitempty" yaml:"maxDelta,omitempty"`
	// SamplesQuery conta le richieste osservate; sotto MinSamples il controllo non è verde.
	SamplesQuery string  `json:"samplesQuery,omitempty" yaml:"samplesQuery,omitempty"`
	MinSamples   float64 `json:"minSamples,omitempty" yaml:"minSamples,omitempty"`
}

// CompareStable confronta la versione candidata con quella stabile.
const CompareStable = "stable"

// AlertGate blocca il passo se in Alertmanager c'è un allarme attivo che soddisfa tutte le
// condizioni di BlockOn (es. "severity=critical") e riguarda l'applicazione, la region o l'AZ.
type AlertGate struct {
	BlockOn []string `json:"blockOn" yaml:"blockOn"`
}

// Durate di default.
const (
	DefaultHealthInterval = 30 * time.Second
	maxBake               = 24 * time.Hour
)

// BakeDuration restituisce il tempo di osservazione dell'ondata.
func (w Wave) BakeDuration() time.Duration {
	d, _ := time.ParseDuration(w.Bake)
	return d
}

// IntervalDuration restituisce la frequenza dei controlli.
func (h *Health) IntervalDuration() time.Duration {
	if h == nil || h.Interval == "" {
		return DefaultHealthInterval
	}
	d, _ := time.ParseDuration(h.Interval)
	return d
}

// MaxUnavailableFor restituisce i target aggiornati insieme su n target (almeno 1).
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

// OnFailureOrDefault restituisce il comportamento in caso di passo fallito.
func (r *Rollout) OnFailureOrDefault() string {
	if r.OnFailure == "" {
		if r.IsTraffic() {
			// Tornare alla versione stabile è uno spostamento di traffico: è il default.
			return OnFailureRollbackAll
		}
		return OnFailureStop
	}
	return r.OnFailure
}

// Validate verifica la struttura del rollout. La corrispondenza con i target si verifica dopo
// la fusione con la configurazione di ambiente (internal/rollout).
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
		if len(r.Waves) > 0 {
			errs = append(errs, errors.New("rollout.waves non è supportato con strategy: traffic"))
		}
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
