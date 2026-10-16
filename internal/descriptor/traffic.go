package descriptor

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Rollout strategies.
const (
	// StrategyWaves updates the targets in waves (default).
	StrategyWaves = "waves"
	// StrategyTraffic releases the new version next to the stable one and shifts traffic by
	// percentages (canary; blue/green with a single 100% step).
	StrategyTraffic = "traffic"
)

// Behaviors when the router weights are not the ones set by manent.
const (
	OnDriftPause  = "pause"
	OnDriftIgnore = "ignore"
)

// ReplicasProportional scales the candidate version in proportion to the step weight.
const ReplicasProportional = "proportional"

// TrafficRollout is the release strategy by traffic percentage.
type TrafficRollout struct {
	// Steps are the increasing traffic percentages towards the new version, the last one 100.
	// [100] is a blue/green: a single switch after the tests at weight 0.
	Steps []int `json:"steps" yaml:"steps"`
	// Bake is the observation time after every step but the last (e.g. "10m").
	Bake string `json:"bake,omitempty" yaml:"bake,omitempty"`
	// Testers routes to the new version, before the first step, only the testers' requests
	// (header or tag, depending on the router).
	Testers *Testers `json:"testers,omitempty" yaml:"testers,omitempty"`
	// ScaleDownDelay is the wait before scaling down the replaced version: until then a rollback
	// is a weight change (default 30m).
	ScaleDownDelay string `json:"scaleDownDelay,omitempty" yaml:"scaleDownDelay,omitempty"`
	// Replicas is "" (same replicas as the stable version) or "proportional".
	Replicas string `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	// OnDrift is pause (default: the rollout stops waiting for a decision) or ignore.
	OnDrift string `json:"onDrift,omitempty" yaml:"onDrift,omitempty"`
}

// Testers identifies the testers' requests.
type Testers struct {
	// Header is "name: value" (e.g. "x-canary: always"), for routers that route by header.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	// Tag is the label of the candidate revision, for routers that give testers a URL
	// (e.g. Cloud Run).
	Tag string `json:"tag,omitempty" yaml:"tag,omitempty"`
}

// DefaultScaleDownDelay is the default wait before scaling down the replaced version.
const DefaultScaleDownDelay = 30 * time.Minute

// StageZero is the approvalsBetween value that asks for an approval after the tests at
// weight 0, before the first traffic shift.
const StageZero = "0"

var (
	headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	tagRe        = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,44}[a-z0-9])?$`)
)

// IsTraffic reports the traffic percentage strategy.
func (r *Rollout) IsTraffic() bool { return r != nil && r.Strategy == StrategyTraffic }

// BakeDuration returns the observation time of a step.
func (t *TrafficRollout) BakeDuration() time.Duration {
	d, _ := time.ParseDuration(t.Bake)
	return d
}

// ScaleDownDelayDuration returns the wait before scaling down.
func (t *TrafficRollout) ScaleDownDelayDuration() time.Duration {
	if t.ScaleDownDelay == "" {
		return DefaultScaleDownDelay
	}
	d, _ := time.ParseDuration(t.ScaleDownDelay)
	return d
}

// HeaderPair returns name and value of the testers' header.
func (t *Testers) HeaderPair() (string, string) {
	if t == nil {
		return "", ""
	}
	k, v, _ := strings.Cut(t.Header, ":")
	return strings.TrimSpace(k), strings.TrimSpace(v)
}

func (t *TrafficRollout) validate() error {
	var errs []error
	if len(t.Steps) == 0 {
		errs = append(errs, errors.New("rollout.traffic.steps: almeno un gradino (es. [1, 5, 25, 50, 100], [100] per un blue/green)"))
	}
	prev := 0
	for i, s := range t.Steps {
		if s <= prev || s > 100 {
			errs = append(errs, fmt.Errorf("rollout.traffic.steps[%d] = %d: le percentuali vanno da 1 a 100, crescenti", i, s))
		}
		prev = s
	}
	if len(t.Steps) > 0 && t.Steps[len(t.Steps)-1] != 100 {
		errs = append(errs, errors.New("rollout.traffic.steps: l'ultimo gradino deve essere 100"))
	}
	if len(t.Steps) > 20 {
		errs = append(errs, errors.New("rollout.traffic.steps: al più 20 gradini"))
	}
	for _, f := range []struct{ name, v string }{{"bake", t.Bake}, {"scaleDownDelay", t.ScaleDownDelay}} {
		if f.v == "" {
			continue
		}
		if d, err := time.ParseDuration(f.v); err != nil || d < 0 || d > maxBake {
			errs = append(errs, fmt.Errorf("rollout.traffic.%s %q non valido (durata tra 0 e 24h)", f.name, f.v))
		}
	}
	switch t.Replicas {
	case "", ReplicasProportional:
	default:
		errs = append(errs, fmt.Errorf("rollout.traffic.replicas %q non valido (proportional)", t.Replicas))
	}
	switch t.OnDrift {
	case "", OnDriftPause, OnDriftIgnore:
	default:
		errs = append(errs, fmt.Errorf("rollout.traffic.onDrift %q non valido (pause|ignore)", t.OnDrift))
	}
	if ts := t.Testers; ts != nil {
		if ts.Header == "" && ts.Tag == "" {
			errs = append(errs, errors.New("rollout.traffic.testers: header o tag"))
		}
		if ts.Header != "" {
			k, v := ts.HeaderPair()
			if !headerNameRe.MatchString(k) || v == "" || strings.ContainsAny(v, "\r\n") {
				errs = append(errs, fmt.Errorf("rollout.traffic.testers.header %q non valido (\"nome: valore\")", ts.Header))
			}
		}
		if ts.Tag != "" && !tagRe.MatchString(ts.Tag) {
			errs = append(errs, fmt.Errorf("rollout.traffic.testers.tag %q non valido", ts.Tag))
		}
	}
	return errors.Join(errs...)
}

// stageNames are the valid approvalsBetween names for the traffic strategy: "0" and the
// percentages of every step but the last.
func (t *TrafficRollout) stageNames() []string {
	out := []string{StageZero}
	for _, s := range t.Steps {
		if s < 100 {
			out = append(out, strconv.Itoa(s))
		}
	}
	return out
}

// TrafficConfig is the integration with an environment's load balancers and routers. It lives
// only in the environment configuration: the descriptor cannot define it.
type TrafficConfig struct {
	// Pool drains the targets from the load balancer before updating them and puts them back in
	// service afterwards.
	Pool *PoolConfig `json:"pool,omitempty" yaml:"pool,omitempty"`
	// Router shifts traffic between two groups of targets (slot label) in percentage rollouts.
	// Without it, the router is the deploy plugin's.
	Router *RouterConfig `json:"router,omitempty" yaml:"router,omitempty"`
}

// Behaviors when draining times out.
const (
	OnTimeoutProceed = "proceed"
	OnTimeoutFail    = "fail"
)

// Default pool durations.
const (
	DefaultDrainTimeout   = 5 * time.Minute
	DefaultRestoreTimeout = 5 * time.Minute
	DefaultPollInterval   = 5 * time.Second
)

// PoolConfig is the pool of a load balancer.
type PoolConfig struct {
	// Provider is haproxy, aws-elb, health-flag, f5, nginx-plus or nginx.
	Provider string `json:"provider" yaml:"provider"`
	// Member is the pool member name of a target: {host}, {port} and {label.<name>} are
	// replaced (default "{host}").
	Member string `json:"member,omitempty" yaml:"member,omitempty"`
	// DrainTimeout is the maximum wait for the remaining connections (default 5m).
	DrainTimeout string `json:"drain_timeout,omitempty" yaml:"drain_timeout,omitempty"`
	// RestoreTimeout is the maximum wait for returning healthy (default 5m).
	RestoreTimeout string `json:"restore_timeout,omitempty" yaml:"restore_timeout,omitempty"`
	// PollInterval is the frequency of reads during waits (default 5s).
	PollInterval string `json:"poll_interval,omitempty" yaml:"poll_interval,omitempty"`
	// MinHealthy are the healthy members that must stay in service: a number or a percentage of
	// the pool members (default 1).
	MinHealthy string `json:"min_healthy,omitempty" yaml:"min_healthy,omitempty"`
	// OnTimeout is proceed (default: the deploy closes the remaining connections) or fail.
	OnTimeout string `json:"on_timeout,omitempty" yaml:"on_timeout,omitempty"`
	// Settings are the provider settings (API URLs, backends, secrets by name).
	Settings map[string]string `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// RouterConfig is the router of traffic between slots.
type RouterConfig struct {
	// Provider is aws-alb, haproxy, f5 or nginx-plus.
	Provider string            `json:"provider" yaml:"provider"`
	Settings map[string]string `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// Durations returns the pool waits.
func (p *PoolConfig) Durations() (drain, restore, poll time.Duration) {
	parse := func(s string, def time.Duration) time.Duration {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		return def
	}
	return parse(p.DrainTimeout, DefaultDrainTimeout), parse(p.RestoreTimeout, DefaultRestoreTimeout), parse(p.PollInterval, DefaultPollInterval)
}

// MinHealthyFor returns the healthy members to keep out of n pool members.
func (p *PoolConfig) MinHealthyFor(n int) int {
	v := strings.TrimSpace(p.MinHealthy)
	if v == "" {
		return min(1, n)
	}
	if pct, ok := strings.CutSuffix(v, "%"); ok {
		k, _ := strconv.Atoi(pct)
		// Rounded up: 66% of 3 members is 2.
		return (n*k + 99) / 100
	}
	k, _ := strconv.Atoi(v)
	return k
}

// MemberName computes the pool member name of a target.
func (p *PoolConfig) MemberName(t Target) string {
	return ExpandTarget(p.Member, t)
}

// ExpandTarget replaces {host}, {port} and {label.<name>} with the target data; an empty
// template means "{host}".
func ExpandTarget(tmpl string, t Target) string {
	if tmpl == "" {
		tmpl = "{host}"
	}
	out := strings.NewReplacer("{host}", t.Host, "{port}", strconv.Itoa(t.Port)).Replace(tmpl)
	for k, v := range t.Labels {
		out = strings.ReplaceAll(out, "{label."+k+"}", v)
	}
	return out
}

var settingKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate checks the structure; internal/traffic checks the provider settings.
func (c *TrafficConfig) Validate() error {
	var errs []error
	if p := c.Pool; p != nil {
		if p.Provider == "" {
			errs = append(errs, errors.New("traffic.pool.provider obbligatorio"))
		}
		for _, f := range []struct{ name, v string }{{"drain_timeout", p.DrainTimeout}, {"restore_timeout", p.RestoreTimeout}, {"poll_interval", p.PollInterval}} {
			if f.v == "" {
				continue
			}
			if d, err := time.ParseDuration(f.v); err != nil || d < time.Second || d > maxBake {
				errs = append(errs, fmt.Errorf("traffic.pool.%s %q non valido (durata tra 1s e 24h)", f.name, f.v))
			}
		}
		if v := strings.TrimSpace(p.MinHealthy); v != "" {
			n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
			if err != nil || n < 0 || (strings.HasSuffix(v, "%") && n > 100) {
				errs = append(errs, fmt.Errorf("traffic.pool.min_healthy %q non valido (numero o percentuale)", v))
			}
		}
		switch p.OnTimeout {
		case "", OnTimeoutProceed, OnTimeoutFail:
		default:
			errs = append(errs, fmt.Errorf("traffic.pool.on_timeout %q non valido (proceed|fail)", p.OnTimeout))
		}
		errs = append(errs, checkSettings("traffic.pool.settings", p.Settings))
	}
	if r := c.Router; r != nil {
		if r.Provider == "" {
			errs = append(errs, errors.New("traffic.router.provider obbligatorio"))
		}
		errs = append(errs, checkSettings("traffic.router.settings", r.Settings))
	}
	return errors.Join(errs...)
}

func checkSettings(field string, s map[string]string) error {
	var errs []error
	for k, v := range s {
		if !settingKeyRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("%s: chiave %q non valida", field, k))
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			errs = append(errs, fmt.Errorf("%s.%s: caratteri di controllo non ammessi", field, k))
		}
	}
	return errors.Join(errs...)
}

// SlotLabel is the target label that names the slot (a or b) in percentage rollouts with a
// router outside the plugin.
const SlotLabel = "slot"

// Slots are the two side-by-side slots.
var Slots = []string{"a", "b"}

// OtherSlot returns the slot other than s ("a" if s is empty).
func OtherSlot(s string) string {
	if s == "a" {
		return "b"
	}
	return "a"
}

// ValidSlot reports a valid slot.
func ValidSlot(s string) bool { return slices.Contains(Slots, s) }
