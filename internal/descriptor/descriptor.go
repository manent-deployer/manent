// Package descriptor definisce il descrittore di deploy e il parsing da YAML (CLI) e JSON (API).
package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/labels"
)

// APIVersion è la versione del formato di descrittore supportata.
const APIVersion = "manent/v1"

// Descriptor rappresenta una richiesta di deploy.
type Descriptor struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	// ID opzionale: se valorizzato è usato come chiave di idempotenza del workflow.
	ID          string      `json:"id,omitempty" yaml:"id,omitempty"`
	Application string      `json:"application" yaml:"application"`
	Environment string      `json:"environment" yaml:"environment"`
	Artifact    Artifact    `json:"artifact" yaml:"artifact"`
	Secrets     []SecretRef `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Deploy      Deploy      `json:"deploy" yaml:"deploy"`
	// Steps attiva o disattiva gli step opzionali per nome.
	Steps    map[string]bool `json:"steps,omitempty" yaml:"steps,omitempty"`
	PostTest PostTest        `json:"postTest,omitempty" yaml:"postTest,omitempty"`
	// Schedule, se presente, pianifica il deploy invece di eseguirlo subito.
	Schedule *Schedule `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	// DryRun esegue la pipeline in simulazione: gli step senza effetti sugli ambienti vengono
	// eseguiti, il plugin si limita alle verifiche e gli altri step vengono saltati.
	DryRun bool `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`
	// Callbacks sono gli endpoint HTTP a cui notificare l'esito degli step e del deploy.
	Callbacks []Callback `json:"callbacks,omitempty" yaml:"callbacks,omitempty"`
	// Rollback richiede o esclude il rollback automatico in caso di fallimento.
	// Se è ammesso lo decide la policy del tipo di artefatto.
	Rollback *Rollback `json:"rollback,omitempty" yaml:"rollback,omitempty"`
	// FreezeOverride è il motivo (es. il numero dell'incidente) con cui il deploy deroga a una
	// finestra di freeze. Richiede un permesso specifico ed è registrato nell'audit.
	FreezeOverride string `json:"freezeOverride,omitempty" yaml:"freezeOverride,omitempty"`
	// Rollout, se presente, rilascia a ondate (region, AZ) con health gate tra un passo e
	// l'altro. Di norma sta nella configurazione di ambiente, autorevole.
	Rollout *Rollout `json:"rollout,omitempty" yaml:"rollout,omitempty"`
}

// Rollback configura il rollback automatico. Nel descrittore effettivo (dopo lo step config)
// è sempre valorizzato con la decisione finale, già verificata con la policy.
type Rollback struct {
	// Enabled attiva o disattiva il rollback; se assente vale il default della policy.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// On elenca gli step il cui fallimento avvia il rollback.
	On []string `json:"on,omitempty" yaml:"on,omitempty"`
	// Locator è la strategia con cui individuare la release a cui tornare (default "plugin").
	Locator string `json:"locator,omitempty" yaml:"locator,omitempty"`
}

// IsEnabled indica se il rollback è attivo.
func (r *Rollback) IsEnabled() bool {
	return r != nil && r.Enabled != nil && *r.Enabled
}

// Triggers indica se il fallimento dello step indicato avvia il rollback.
func (r *Rollback) Triggers(step string) bool {
	return r.IsEnabled() && slices.Contains(r.On, step)
}

// validate controlla i nomi degli step e del locator.
func (r *Rollback) validate() error {
	var errs []error
	seen := map[string]bool{}
	for i, s := range r.On {
		if s == "" || seen[s] {
			errs = append(errs, fmt.Errorf("rollback.on[%d]: nome di step vuoto o duplicato", i))
		}
		seen[s] = true
	}
	if r.Locator != "" && !validName(r.Locator) {
		errs = append(errs, fmt.Errorf("rollback.locator %q non valido: %s", r.Locator, nameRule))
	}
	return errors.Join(errs...)
}

// Tipi di evento notificati ai callback.
const (
	// CallbackEventStep è l'esito di un singolo step (anche config).
	CallbackEventStep = "step"
	// CallbackEventDeployment è l'avvio e l'esito finale del deploy.
	CallbackEventDeployment = "deployment"
)

// Tipi di autenticazione dei callback.
const (
	CallbackAuthBearer = "bearer" // Authorization: Bearer <segreto>
	CallbackAuthBasic  = "basic"  // Authorization: Basic <username>:<segreto>
	CallbackAuthHeader = "header" // <header>: <segreto>
	CallbackAuthHMAC   = "hmac"   // firma HMAC-SHA256 del body con il segreto
)

// Callback è un endpoint HTTP che riceve in POST gli eventi del deploy.
type Callback struct {
	URL string `json:"url" yaml:"url"`
	// Events filtra gli eventi inviati ("step", "deployment"); vuoto = tutti.
	Events []string      `json:"events,omitempty" yaml:"events,omitempty"`
	Auth   *CallbackAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// CallbackAuth descrive l'autenticazione verso il callback. Il descrittore è persistito da DBOS:
// contiene solo il nome logico del segreto (sezione secrets), risolto al momento dell'invio.
type CallbackAuth struct {
	Type   string `json:"type" yaml:"type"`
	Secret string `json:"secret" yaml:"secret"`
	// Username per il tipo basic.
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	// Header è il nome dell'header per il tipo header (es. X-Api-Key).
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
}

// Wants indica se il callback deve ricevere gli eventi del tipo indicato.
func (c Callback) Wants(event string) bool {
	return len(c.Events) == 0 || slices.Contains(c.Events, event)
}

// validate controlla URL, eventi e autenticazione del callback.
func (c Callback) validate(i int) error {
	var errs []error
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		errs = append(errs, fmt.Errorf("callbacks[%d].url deve essere un URL http(s) assoluto", i))
	case u.User != nil:
		// Le credenziali nell'URL verrebbero persistite: vanno passate con auth.
		errs = append(errs, fmt.Errorf("callbacks[%d].url non deve contenere credenziali, usare auth", i))
	}
	for _, e := range c.Events {
		if e != CallbackEventStep && e != CallbackEventDeployment {
			errs = append(errs, fmt.Errorf("callbacks[%d].events: %q non valido (step|deployment)", i, e))
		}
	}
	if a := c.Auth; a != nil {
		switch a.Type {
		case CallbackAuthBearer, CallbackAuthHMAC:
		case CallbackAuthBasic:
			if a.Username == "" {
				errs = append(errs, fmt.Errorf("callbacks[%d].auth.username obbligatorio con basic", i))
			}
		case CallbackAuthHeader:
			if a.Header == "" {
				errs = append(errs, fmt.Errorf("callbacks[%d].auth.header obbligatorio con header", i))
			}
		default:
			errs = append(errs, fmt.Errorf("callbacks[%d].auth.type %q non valido (bearer|basic|header|hmac)", i, a.Type))
		}
		if a.Secret == "" {
			errs = append(errs, fmt.Errorf("callbacks[%d].auth.secret obbligatorio: nome di un segreto della sezione secrets", i))
		}
	}
	return errors.Join(errs...)
}

// Schedule pianifica il deploy: At per un'esecuzione singola, Cron per esecuzioni ricorrenti.
type Schedule struct {
	// At è l'orario di un deploy singolo (RFC 3339).
	At *time.Time `json:"at,omitempty" yaml:"at,omitempty"`
	// Cron è l'espressione per i deploy ricorrenti, a 5 campi o a 6 campi con i secondi.
	Cron string `json:"cron,omitempty" yaml:"cron,omitempty"`
	// Timezone IANA con cui interpretare Cron (default: fuso dell'executor).
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Name identifica la schedule ricorrente; default "<application>-<environment>".
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}

// Artifact descrive l'artefatto da rilasciare.
type Artifact struct {
	Type    string `json:"type" yaml:"type"`
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
	// Source indica dove recuperare l'artefatto (percorso locale, file://, http(s)://) o
	// l'immagine di un registry (oci://registry/repository:tag, oci+http:// per un registry in chiaro).
	Source string `json:"source" yaml:"source"`
	// SHA256 atteso, verificato dallo step di QA se presente.
	SHA256 string `json:"sha256,omitempty" yaml:"sha256,omitempty"`
}

// SecretRef associa un nome logico a un riferimento del secret provider (es. "env:VAR").
type SecretRef struct {
	Name string `json:"name" yaml:"name"`
	Ref  string `json:"ref" yaml:"ref"`
}

// Deploy contiene la policy, l'eventuale plugin esplicito, i target e i parametri del plugin.
// Tutti i campi possono arrivare dalla configurazione di ambiente (internal/envconfig):
// il descrittore può solo aggiungere ciò che lì non è definito.
type Deploy struct {
	Policy  string            `json:"policy,omitempty" yaml:"policy,omitempty"`
	Plugin  string            `json:"plugin,omitempty" yaml:"plugin,omitempty"`
	Targets []Target          `json:"targets,omitempty" yaml:"targets,omitempty"`
	Params  map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
}

// Target è una destinazione del deploy.
type Target struct {
	Host string `json:"host" yaml:"host"`
	Port int    `json:"port,omitempty" yaml:"port,omitempty"`
	User string `json:"user,omitempty" yaml:"user,omitempty"`
	// Labels classificano il target; region e az sono riservate e usate dal rollout a ondate.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// Equal confronta due target, etichette comprese.
func (t Target) Equal(o Target) bool {
	return t.Host == o.Host && t.Port == o.Port && t.User == o.User && maps.Equal(t.Labels, o.Labels)
}

// PostTest raccoglie i controlli da eseguire dopo il deploy.
type PostTest struct {
	HTTP []HTTPCheck `json:"http,omitempty" yaml:"http,omitempty"`
}

// HTTPCheck verifica che un endpoint risponda con lo status atteso.
type HTTPCheck struct {
	URL          string `json:"url" yaml:"url"`
	ExpectStatus int    `json:"expectStatus,omitempty" yaml:"expectStatus,omitempty"`
}

// ParseYAML legge un descrittore YAML, rifiutando campi sconosciuti.
func ParseYAML(data []byte) (*Descriptor, error) {
	var d Descriptor
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("descrittore YAML non valido: %w", err)
	}
	return &d, d.Validate()
}

// ParseJSON legge un descrittore JSON, rifiutando campi sconosciuti.
func ParseJSON(data []byte) (*Descriptor, error) {
	var d Descriptor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("descrittore JSON non valido: %w", err)
	}
	return &d, d.Validate()
}

// LoadYAMLFile legge e valida un descrittore YAML da file.
func LoadYAMLFile(path string) (*Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseYAML(data)
}

// Validate controlla i campi obbligatori del descrittore.
func (d *Descriptor) Validate() error {
	var errs []error
	if d.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion deve essere %q", APIVersion))
	}
	if !validName(d.Application) {
		errs = append(errs, fmt.Errorf("application %q non valido: %s", d.Application, nameRule))
	}
	if !validName(d.Environment) {
		errs = append(errs, fmt.Errorf("environment %q non valido: %s", d.Environment, nameRule))
	}
	if d.Artifact.Type == "" || d.Artifact.Name == "" || d.Artifact.Source == "" {
		errs = append(errs, errors.New("artifact.type, artifact.name e artifact.source obbligatori"))
	}
	for i, t := range d.Deploy.Targets {
		if t.Host == "" {
			errs = append(errs, fmt.Errorf("deploy.targets[%d].host obbligatorio", i))
		}
		if err := labels.ValidLabels(t.Labels); err != nil {
			errs = append(errs, fmt.Errorf("deploy.targets[%d]: %w", i, err))
		}
	}
	if d.Rollout != nil {
		errs = append(errs, d.Rollout.Validate())
	}
	seen := map[string]bool{}
	for i, s := range d.Secrets {
		if s.Name == "" || s.Ref == "" {
			errs = append(errs, fmt.Errorf("secrets[%d]: name e ref obbligatori", i))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Errorf("secrets[%d]: nome %q duplicato", i, s.Name))
		}
		seen[s.Name] = true
	}
	if len(d.Callbacks) > maxCallbacks {
		errs = append(errs, fmt.Errorf("callbacks: al massimo %d", maxCallbacks))
	}
	for i, c := range d.Callbacks {
		errs = append(errs, c.validate(i))
	}
	if d.Rollback != nil {
		errs = append(errs, d.Rollback.validate())
	}
	if len(d.FreezeOverride) > maxOverride {
		errs = append(errs, fmt.Errorf("freezeOverride: al massimo %d caratteri", maxOverride))
	}
	if d.FreezeOverride != "" && d.Schedule != nil && d.Schedule.Cron != "" {
		errs = append(errs, errors.New("freezeOverride non ammesso con schedule.cron: la deroga vale per un singolo deploy"))
	}
	if d.ID != "" && !validName(d.ID) {
		errs = append(errs, fmt.Errorf("id %q non valido: %s", d.ID, nameRule))
	}
	if d.Schedule != nil {
		if d.DryRun {
			errs = append(errs, errors.New("dryRun non ammesso con schedule: il dry-run viene eseguito subito"))
		}
		errs = append(errs, d.Schedule.validate(d.ID))
		if d.Schedule.Cron != "" && !validName(d.ScheduleName()) {
			errs = append(errs, fmt.Errorf("nome schedule %q non valido: %s", d.ScheduleName(), nameRule))
		}
	}
	return errors.Join(errs...)
}

// maxOverride limita la lunghezza del motivo di una deroga al freeze.
const maxOverride = 500

// maxCallbacks limita i callback di un deploy: ogni evento genera una chiamata per callback.
const maxCallbacks = 5

// nameRe limita ID e nomi di schedule, usati anche come nomi di directory sul worker.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const nameRule = "ammessi lettere, cifre, '.', '_', ':', '-' (max 128, iniziale alfanumerica)"

func validName(s string) bool { return nameRe.MatchString(s) }

// validate controlla che sia indicato esattamente uno tra At e Cron e che il cron sia valido.
func (s *Schedule) validate(id string) error {
	switch {
	case s.At == nil && s.Cron == "":
		return errors.New("schedule: indicare at oppure cron")
	case s.At != nil && s.Cron != "":
		return errors.New("schedule: at e cron sono alternativi")
	case s.At != nil && (s.Timezone != "" || s.Name != ""):
		return errors.New("schedule: timezone e name si usano solo con cron")
	case s.Cron != "" && id != "":
		return errors.New("schedule: id non ammesso con cron, ogni esecuzione riceve un ID derivato dal nome della schedule")
	case s.Cron != "":
		expr := s.CronExpr()
		if s.Timezone != "" {
			expr = "CRON_TZ=" + s.Timezone + " " + expr
		}
		if _, err := cronParser.Parse(expr); err != nil {
			return fmt.Errorf("schedule.cron non valido: %w", err)
		}
	}
	return nil
}

// cronParser accetta lo stesso formato di DBOS (6 campi, con secondi).
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// CronExpr restituisce l'espressione nel formato a 6 campi richiesto da DBOS,
// aggiungendo "0" come campo dei secondi alle espressioni classiche a 5 campi.
func (s *Schedule) CronExpr() string {
	expr := strings.TrimSpace(s.Cron)
	if len(strings.Fields(expr)) == 5 {
		return "0 " + expr
	}
	return expr
}

// ScheduleName restituisce il nome della schedule ricorrente.
func (d *Descriptor) ScheduleName() string {
	if d.Schedule != nil && d.Schedule.Name != "" {
		return d.Schedule.Name
	}
	return d.Application + "-" + d.Environment
}

// SecretRefByName restituisce il riferimento associato a un nome logico.
func (d *Descriptor) SecretRefByName(name string) (string, bool) {
	for _, s := range d.Secrets {
		if s.Name == name {
			return s.Ref, true
		}
	}
	return "", false
}
