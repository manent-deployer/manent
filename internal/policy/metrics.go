package policy

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// collector exposes the state of the policy to Prometheus.
type collector struct {
	r           *Runtime
	evaluations *prometheus.CounterVec
	durations   *prometheus.HistogramVec
	loadErrors  prometheus.Counter
	authz       authzMetrics
}

var (
	descRevision = prometheus.NewDesc("manent_policy_revision_info", "Revisione della policy di governance in vigore (valore 1).",
		[]string{"engine", "source", "revision"}, nil)
	descAge = prometheus.NewDesc("manent_policy_age_seconds", "Secondi dall'ultimo aggiornamento riuscito della policy e dei freeze.", nil, nil)
)

func newCollector(r *Runtime) *collector {
	return &collector{
		r: r,
		evaluations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "manent_policy_evaluations_total", Help: "Valutazioni della policy di governance per fase ed esito.",
		}, []string{"phase", "outcome"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "manent_policy_eval_duration_seconds", Help: "Durata delle valutazioni della policy di governance.",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
		}, []string{"phase"}),
		loadErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "manent_policy_load_errors_total", Help: "Aggiornamenti della policy o dei freeze falliti (revisione rifiutata o sorgente irraggiungibile).",
		}),
		authz: newAuthzMetrics(),
	}
}

// Collector returns the Prometheus collector of the policy.
func (r *Runtime) Collector() prometheus.Collector { return r.metrics }

func (c *collector) evaluation(phase, outcome string, d time.Duration) {
	c.evaluations.WithLabelValues(phase, outcome).Inc()
	if d > 0 {
		c.durations.WithLabelValues(phase).Observe(d.Seconds())
	}
}

func (c *collector) loadError() { c.loadErrors.Inc() }

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descRevision
	ch <- descAge
	c.evaluations.Describe(ch)
	c.durations.Describe(ch)
	c.loadErrors.Describe(ch)
	if c.r.o.Compile.Authz != nil {
		ch <- descAuthzCache
		c.authz.grantEvals.Describe(ch)
		c.authz.denyEvals.Describe(ch)
		c.authz.durations.Describe(ch)
		c.authz.invalidGrants.Describe(ch)
	}
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	st := c.r.state.Load()
	ch <- prometheus.MustNewConstMetric(descRevision, prometheus.GaugeValue, 1, c.r.o.Engine, c.r.o.SourceName, c.r.Revision())
	if !st.okAt.IsZero() {
		ch <- prometheus.MustNewConstMetric(descAge, prometheus.GaugeValue, c.r.now().Sub(st.okAt).Seconds())
	}
	c.evaluations.Collect(ch)
	c.durations.Collect(ch)
	c.loadErrors.Collect(ch)
	if c.r.o.Compile.Authz != nil {
		ch <- prometheus.MustNewConstMetric(descAuthzCache, prometheus.GaugeValue, float64(c.r.grants.len()))
		c.authz.grantEvals.Collect(ch)
		c.authz.denyEvals.Collect(ch)
		c.authz.durations.Collect(ch)
		c.authz.invalidGrants.Collect(ch)
	}
}
