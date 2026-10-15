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

// Strategie di rollout.
const (
	// StrategyWaves aggiorna i target a ondate (default).
	StrategyWaves = "waves"
	// StrategyTraffic rilascia la nuova versione accanto a quella stabile e sposta il traffico
	// per percentuali (canary; blue/green con un solo gradino al 100%).
	StrategyTraffic = "traffic"
)

// Comportamenti quando i pesi del router non sono quelli impostati da manent.
const (
	OnDriftPause  = "pause"
	OnDriftIgnore = "ignore"
)

// ReplicasProportional scala la versione candidata in proporzione al peso del gradino.
const ReplicasProportional = "proportional"

// TrafficRollout è la strategia di rilascio per percentuale di traffico.
type TrafficRollout struct {
	// Steps sono le percentuali di traffico verso la nuova versione, crescenti, l'ultima 100.
	// [100] è un blue/green: un solo switch dopo i test a peso 0.
	Steps []int `json:"steps" yaml:"steps"`
	// Bake è il tempo di osservazione dopo ogni gradino tranne l'ultimo (es. "10m").
	Bake string `json:"bake,omitempty" yaml:"bake,omitempty"`
	// Testers instrada verso la nuova versione, prima del primo gradino, solo le richieste dei
	// tester (header o tag, secondo il router).
	Testers *Testers `json:"testers,omitempty" yaml:"testers,omitempty"`
	// ScaleDownDelay è l'attesa prima di ridimensionare la versione sostituita: fino ad allora
	// un rollback è un cambio di peso (default 30m).
	ScaleDownDelay string `json:"scaleDownDelay,omitempty" yaml:"scaleDownDelay,omitempty"`
	// Replicas è "" (stesse repliche della versione stabile) o "proportional".
	Replicas string `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	// OnDrift è pause (default: il rollout si ferma in attesa di una decisione) o ignore.
	OnDrift string `json:"onDrift,omitempty" yaml:"onDrift,omitempty"`
}

// Testers identifica le richieste dei tester.
type Testers struct {
	// Header è "nome: valore" (es. "x-canary: always"), per i router che instradano per header.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	// Tag è l'etichetta della revisione candidata, per i router che danno un URL ai tester
	// (es. Cloud Run).
	Tag string `json:"tag,omitempty" yaml:"tag,omitempty"`
}

// DefaultScaleDownDelay è l'attesa di default prima di ridimensionare la versione sostituita.
const DefaultScaleDownDelay = 30 * time.Minute

// StageZero è il valore di approvalsBetween che chiede un'approvazione dopo i test a peso 0,
// prima del primo spostamento di traffico.
const StageZero = "0"

var (
	headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	tagRe        = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,44}[a-z0-9])?$`)
)

// IsTraffic indica la strategia per percentuale di traffico.
func (r *Rollout) IsTraffic() bool { return r != nil && r.Strategy == StrategyTraffic }

// BakeDuration restituisce il tempo di osservazione di un gradino.
func (t *TrafficRollout) BakeDuration() time.Duration {
	d, _ := time.ParseDuration(t.Bake)
	return d
}

// ScaleDownDelayDuration restituisce l'attesa prima del ridimensionamento.
func (t *TrafficRollout) ScaleDownDelayDuration() time.Duration {
	if t.ScaleDownDelay == "" {
		return DefaultScaleDownDelay
	}
	d, _ := time.ParseDuration(t.ScaleDownDelay)
	return d
}

// HeaderPair restituisce nome e valore dell'header dei tester.
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

// stageNames sono i nomi validi in approvalsBetween per la strategia traffic: "0" e le
// percentuali dei gradini tranne l'ultimo.
func (t *TrafficRollout) stageNames() []string {
	out := []string{StageZero}
	for _, s := range t.Steps {
		if s < 100 {
			out = append(out, strconv.Itoa(s))
		}
	}
	return out
}

// TrafficConfig è l'integrazione con load balancer e router di un ambiente. Sta solo nella
// configurazione di ambiente: il descrittore non può definirla.
type TrafficConfig struct {
	// Pool svuota i target dal load balancer prima di aggiornarli e li rimette in servizio dopo.
	Pool *PoolConfig `json:"pool,omitempty" yaml:"pool,omitempty"`
	// Router sposta il traffico tra due gruppi di target (etichetta slot) nei rilasci per
	// percentuale. Senza, il router è quello del plugin di deploy.
	Router *RouterConfig `json:"router,omitempty" yaml:"router,omitempty"`
}

// Comportamenti allo scadere dello svuotamento.
const (
	OnTimeoutProceed = "proceed"
	OnTimeoutFail    = "fail"
)

// Durate di default del pool.
const (
	DefaultDrainTimeout   = 5 * time.Minute
	DefaultRestoreTimeout = 5 * time.Minute
	DefaultPollInterval   = 5 * time.Second
)

// PoolConfig è il pool di un load balancer.
type PoolConfig struct {
	// Provider è haproxy, aws-elb, health-flag, f5, nginx-plus o nginx.
	Provider string `json:"provider" yaml:"provider"`
	// Member è il nome del membro del pool di un target: {host}, {port} e {label.<nome>}
	// vengono sostituiti (default "{host}").
	Member string `json:"member,omitempty" yaml:"member,omitempty"`
	// DrainTimeout è l'attesa massima delle connessioni residue (default 5m).
	DrainTimeout string `json:"drain_timeout,omitempty" yaml:"drain_timeout,omitempty"`
	// RestoreTimeout è l'attesa massima del rientro in salute (default 5m).
	RestoreTimeout string `json:"restore_timeout,omitempty" yaml:"restore_timeout,omitempty"`
	// PollInterval è la frequenza delle letture durante le attese (default 5s).
	PollInterval string `json:"poll_interval,omitempty" yaml:"poll_interval,omitempty"`
	// MinHealthy sono i membri sani che devono restare in servizio: un numero o una
	// percentuale dei membri del pool (default 1).
	MinHealthy string `json:"min_healthy,omitempty" yaml:"min_healthy,omitempty"`
	// OnTimeout è proceed (default: il deploy chiude le connessioni residue) o fail.
	OnTimeout string `json:"on_timeout,omitempty" yaml:"on_timeout,omitempty"`
	// Settings sono le impostazioni del provider (URL delle API, backend, segreti per nome).
	Settings map[string]string `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// RouterConfig è il router del traffico tra gli slot.
type RouterConfig struct {
	// Provider è aws-alb, haproxy, f5 o nginx-plus.
	Provider string            `json:"provider" yaml:"provider"`
	Settings map[string]string `json:"settings,omitempty" yaml:"settings,omitempty"`
}

// Durations restituisce le attese del pool.
func (p *PoolConfig) Durations() (drain, restore, poll time.Duration) {
	parse := func(s string, def time.Duration) time.Duration {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		return def
	}
	return parse(p.DrainTimeout, DefaultDrainTimeout), parse(p.RestoreTimeout, DefaultRestoreTimeout), parse(p.PollInterval, DefaultPollInterval)
}

// MinHealthyFor restituisce i membri sani da conservare su n membri del pool.
func (p *PoolConfig) MinHealthyFor(n int) int {
	v := strings.TrimSpace(p.MinHealthy)
	if v == "" {
		return min(1, n)
	}
	if pct, ok := strings.CutSuffix(v, "%"); ok {
		k, _ := strconv.Atoi(pct)
		// Arrotondato per eccesso: 66% di 3 membri sono 2.
		return (n*k + 99) / 100
	}
	k, _ := strconv.Atoi(v)
	return k
}

// MemberName calcola il nome del membro del pool di un target.
func (p *PoolConfig) MemberName(t Target) string {
	return ExpandTarget(p.Member, t)
}

// ExpandTarget sostituisce {host}, {port} e {label.<nome>} con i dati del target; un modello
// vuoto vale "{host}".
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

// Validate verifica la struttura; le impostazioni del provider le verifica internal/traffic.
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

// SlotLabel è l'etichetta dei target che indica lo slot (a o b) nei rilasci per percentuale
// con un router esterno al plugin.
const SlotLabel = "slot"

// Slots sono i due slot affiancati.
var Slots = []string{"a", "b"}

// OtherSlot restituisce lo slot diverso da s ("a" se s è vuoto).
func OtherSlot(s string) string {
	if s == "a" {
		return "b"
	}
	return "a"
}

// ValidSlot indica uno slot valido.
func ValidSlot(s string) bool { return slices.Contains(Slots, s) }
