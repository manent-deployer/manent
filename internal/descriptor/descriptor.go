// Package descriptor definisce il descrittore di deploy e il parsing da YAML (CLI) e JSON (API).
package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// APIVersion è la versione del formato di descrittore supportata.
const APIVersion = "go-dart/v1"

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
	// Source indica dove recuperare l'artefatto (percorso locale, file://, http(s)://).
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
type Deploy struct {
	Policy  string            `json:"policy,omitempty" yaml:"policy,omitempty"`
	Plugin  string            `json:"plugin,omitempty" yaml:"plugin,omitempty"`
	Targets []Target          `json:"targets" yaml:"targets"`
	Params  map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
}

// Target è una destinazione del deploy.
type Target struct {
	Host string `json:"host" yaml:"host"`
	Port int    `json:"port,omitempty" yaml:"port,omitempty"`
	User string `json:"user,omitempty" yaml:"user,omitempty"`
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
	if d.Application == "" {
		errs = append(errs, errors.New("application obbligatorio"))
	}
	if d.Environment == "" {
		errs = append(errs, errors.New("environment obbligatorio"))
	}
	if d.Artifact.Type == "" || d.Artifact.Name == "" || d.Artifact.Source == "" {
		errs = append(errs, errors.New("artifact.type, artifact.name e artifact.source obbligatori"))
	}
	if len(d.Deploy.Targets) == 0 {
		errs = append(errs, errors.New("deploy.targets deve contenere almeno un target"))
	}
	for i, t := range d.Deploy.Targets {
		if t.Host == "" {
			errs = append(errs, fmt.Errorf("deploy.targets[%d].host obbligatorio", i))
		}
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
	if d.ID != "" && !validName(d.ID) {
		errs = append(errs, fmt.Errorf("id %q non valido: %s", d.ID, nameRule))
	}
	if d.Schedule != nil {
		errs = append(errs, d.Schedule.validate(d.ID))
		if d.Schedule.Cron != "" && !validName(d.ScheduleName()) {
			errs = append(errs, fmt.Errorf("nome schedule %q non valido: %s", d.ScheduleName(), nameRule))
		}
	}
	return errors.Join(errs...)
}

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
