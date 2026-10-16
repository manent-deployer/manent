// Package descriptor defines the deploy descriptor and its parsing from YAML (CLI) and JSON (API).
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

// APIVersion is the supported descriptor format version.
const APIVersion = "manent/v1"

// Descriptor is a deploy request.
type Descriptor struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	// ID is optional: if set it is the idempotency key of the workflow.
	ID          string      `json:"id,omitempty" yaml:"id,omitempty"`
	Application string      `json:"application" yaml:"application"`
	Environment string      `json:"environment" yaml:"environment"`
	Artifact    Artifact    `json:"artifact" yaml:"artifact"`
	Secrets     []SecretRef `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Deploy      Deploy      `json:"deploy" yaml:"deploy"`
	// Steps enables or disables optional steps by name.
	Steps map[string]bool `json:"steps,omitempty" yaml:"steps,omitempty"`
	// StepParams are the parameters of custom steps, per step. They normally come from the
	// environment configuration: the descriptor can set only those allowed by
	// descriptor_policy.step_params.
	StepParams map[string]map[string]string `json:"stepParams,omitempty" yaml:"stepParams,omitempty"`
	PostTest   PostTest                     `json:"postTest,omitempty" yaml:"postTest,omitempty"`
	// Schedule, if set, schedules the deploy instead of running it at once.
	Schedule *Schedule `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	// DryRun runs the pipeline as a simulation: steps without effects on the environments run,
	// the plugin only performs checks and the other steps are skipped.
	DryRun bool `json:"dryRun,omitempty" yaml:"dryRun,omitempty"`
	// Callbacks are the HTTP endpoints to notify of the outcome of steps and deploy.
	Callbacks []Callback `json:"callbacks,omitempty" yaml:"callbacks,omitempty"`
	// Rollback requests or excludes automatic rollback on failure.
	// Whether it is allowed is decided by the artifact type policy.
	Rollback *Rollback `json:"rollback,omitempty" yaml:"rollback,omitempty"`
	// FreezeOverride is the reason (e.g. the incident number) for which the deploy overrides a
	// freeze window. It requires a specific permission and is recorded in the audit.
	FreezeOverride string `json:"freezeOverride,omitempty" yaml:"freezeOverride,omitempty"`
	// Rollout, if set, releases in waves (regions, AZs) with a health gate between passes. It
	// normally lives in the environment configuration, which is authoritative.
	Rollout *Rollout `json:"rollout,omitempty" yaml:"rollout,omitempty"`
	// After lists the IDs of the deploys that must end successfully before this one starts.
	// Until then the deploy waits without taking the environment queue.
	After []string `json:"after,omitempty" yaml:"after,omitempty"`
	// AfterTimeout is the maximum wait for the dependencies (e.g. "2h", default 24h).
	AfterTimeout string `json:"afterTimeout,omitempty" yaml:"afterTimeout,omitempty"`
	// Traffic is the integration with the environment's load balancers and routers. It comes
	// only from the environment configuration (envconfig.Merge rejects a descriptor defining it).
	Traffic *TrafficConfig `json:"traffic,omitempty" yaml:"traffic,omitempty"`
}

// Rollback configures the automatic rollback. In the effective descriptor (after the config
// step) it is always set with the final decision, already checked against the policy.
type Rollback struct {
	// Enabled enables or disables rollback; if absent the policy default applies.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// On lists the steps whose failure starts the rollback.
	On []string `json:"on,omitempty" yaml:"on,omitempty"`
	// Locator is the strategy to find the release to go back to (default "plugin").
	Locator string `json:"locator,omitempty" yaml:"locator,omitempty"`
}

// IsEnabled reports whether rollback is enabled.
func (r *Rollback) IsEnabled() bool {
	return r != nil && r.Enabled != nil && *r.Enabled
}

// Triggers reports whether the failure of the given step starts the rollback.
func (r *Rollback) Triggers(step string) bool {
	return r.IsEnabled() && slices.Contains(r.On, step)
}

// validate checks the step and locator names.
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

// Event types notified to callbacks.
const (
	// CallbackEventStep is the outcome of a single step (config included).
	CallbackEventStep = "step"
	// CallbackEventDeployment is the start and final outcome of the deploy.
	CallbackEventDeployment = "deployment"
)

// Callback authentication types.
const (
	CallbackAuthBearer = "bearer" // Authorization: Bearer <secret>
	CallbackAuthBasic  = "basic"  // Authorization: Basic <username>:<secret>
	CallbackAuthHeader = "header" // <header>: <secret>
	CallbackAuthHMAC   = "hmac"   // HMAC-SHA256 signature of the body with the secret
)

// Callback is an HTTP endpoint that receives the deploy events via POST.
type Callback struct {
	URL string `json:"url" yaml:"url"`
	// Events filters the events sent ("step", "deployment"); empty = all.
	Events []string      `json:"events,omitempty" yaml:"events,omitempty"`
	Auth   *CallbackAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// CallbackAuth describes the authentication towards the callback. DBOS persists the
// descriptor: it holds only the logical name of the secret (secrets section), resolved at
// sending time.
type CallbackAuth struct {
	Type   string `json:"type" yaml:"type"`
	Secret string `json:"secret" yaml:"secret"`
	// Username for the basic type.
	Username string `json:"username,omitempty" yaml:"username,omitempty"`
	// Header is the header name for the header type (e.g. X-Api-Key).
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
}

// Wants reports whether the callback must receive events of the given type.
func (c Callback) Wants(event string) bool {
	return len(c.Events) == 0 || slices.Contains(c.Events, event)
}

// validate checks URL, events and authentication of the callback.
func (c Callback) validate(i int) error {
	var errs []error
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		errs = append(errs, fmt.Errorf("callbacks[%d].url deve essere un URL http(s) assoluto", i))
	case u.User != nil:
		// Credentials in the URL would be persisted: pass them with auth.
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

// Schedule schedules the deploy: At for a single run, Cron for recurring runs.
type Schedule struct {
	// At is the time of a single deploy (RFC 3339).
	At *time.Time `json:"at,omitempty" yaml:"at,omitempty"`
	// Cron is the expression for recurring deploys, with 5 fields or 6 fields with seconds.
	Cron string `json:"cron,omitempty" yaml:"cron,omitempty"`
	// Timezone is the IANA zone to interpret Cron in (default: the executor's zone).
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Name identifies the recurring schedule; default "<application>-<environment>".
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}

// Artifact describes the artifact to release.
type Artifact struct {
	Type    string `json:"type" yaml:"type"`
	Name    string `json:"name" yaml:"name"`
	Version string `json:"version" yaml:"version"`
	// Source says where to fetch the artifact (local path, file://, http(s)://) or a registry
	// image (oci://registry/repository:tag, oci+http:// for a plain-text registry).
	Source string `json:"source" yaml:"source"`
	// SHA256 expected, verified by the QA step if present.
	SHA256 string `json:"sha256,omitempty" yaml:"sha256,omitempty"`
}

// SecretRef maps a logical name to a secret provider reference (e.g. "env:VAR").
type SecretRef struct {
	Name string `json:"name" yaml:"name"`
	Ref  string `json:"ref" yaml:"ref"`
}

// Deploy holds the policy, the explicit plugin if any, the targets and the plugin parameters.
// All fields can come from the environment configuration (internal/envconfig): the
// descriptor can only add what is not defined there.
type Deploy struct {
	Policy  string            `json:"policy,omitempty" yaml:"policy,omitempty"`
	Plugin  string            `json:"plugin,omitempty" yaml:"plugin,omitempty"`
	Targets []Target          `json:"targets,omitempty" yaml:"targets,omitempty"`
	Params  map[string]string `json:"params,omitempty" yaml:"params,omitempty"`
}

// Target is a deploy destination.
type Target struct {
	Host string `json:"host" yaml:"host"`
	Port int    `json:"port,omitempty" yaml:"port,omitempty"`
	User string `json:"user,omitempty" yaml:"user,omitempty"`
	// Labels classify the target; region and az are reserved and used by wave rollouts.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// Equal compares two targets, labels included.
func (t Target) Equal(o Target) bool {
	return t.Host == o.Host && t.Port == o.Port && t.User == o.User && maps.Equal(t.Labels, o.Labels)
}

// PostTest collects the checks to run after the deploy.
type PostTest struct {
	HTTP []HTTPCheck `json:"http,omitempty" yaml:"http,omitempty"`
}

// HTTPCheck checks that an endpoint answers with the expected status.
type HTTPCheck struct {
	URL          string `json:"url" yaml:"url"`
	ExpectStatus int    `json:"expectStatus,omitempty" yaml:"expectStatus,omitempty"`
}

// ParseYAML reads a YAML descriptor, rejecting unknown fields.
func ParseYAML(data []byte) (*Descriptor, error) {
	var d Descriptor
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("descrittore YAML non valido: %w", err)
	}
	return &d, d.Validate()
}

// ParseJSON reads a JSON descriptor, rejecting unknown fields.
func ParseJSON(data []byte) (*Descriptor, error) {
	var d Descriptor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("descrittore JSON non valido: %w", err)
	}
	return &d, d.Validate()
}

// LoadYAMLFile reads and validates a YAML descriptor from a file.
func LoadYAMLFile(path string) (*Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseYAML(data)
}

// Validate checks the mandatory fields of the descriptor.
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
	if d.Traffic != nil {
		errs = append(errs, errors.New("traffic si configura solo nella configurazione di ambiente, non nel descrittore"))
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
	for _, step := range slices.Sorted(maps.Keys(d.StepParams)) {
		for k, v := range d.StepParams[step] {
			if k == "" || len(v) > maxStepParam || strings.ContainsRune(v, 0) {
				errs = append(errs, fmt.Errorf("stepParams.%s.%s: nome vuoto o valore non valido (max %d caratteri)", step, k, maxStepParam))
			}
		}
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
	errs = append(errs, d.validateAfter())
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

// Limits of dependencies between deploys.
const (
	maxAfter            = 20
	DefaultAfterTimeout = 24 * time.Hour
	maxAfterTimeout     = 30 * 24 * time.Hour
)

// validateAfter checks the dependencies: valid and distinct IDs, never the deploy itself, not
// with dry-run nor with schedule.cron (every tick would always depend on the same deploys).
func (d *Descriptor) validateAfter() error {
	if len(d.After) == 0 {
		if d.AfterTimeout != "" {
			return errors.New("afterTimeout si usa solo con after")
		}
		return nil
	}
	var errs []error
	if len(d.After) > maxAfter {
		errs = append(errs, fmt.Errorf("after: al massimo %d dipendenze", maxAfter))
	}
	seen := map[string]bool{}
	for i, id := range d.After {
		switch {
		case !validName(id):
			errs = append(errs, fmt.Errorf("after[%d] %q non valido: %s", i, id, nameRule))
		case seen[id]:
			errs = append(errs, fmt.Errorf("after[%d]: %q duplicato", i, id))
		case d.ID != "" && id == d.ID:
			errs = append(errs, fmt.Errorf("after[%d]: un deploy non può dipendere da se stesso", i))
		}
		seen[id] = true
	}
	if d.DryRun {
		errs = append(errs, errors.New("after non ammesso con dryRun: il dry-run viene eseguito subito"))
	}
	if d.Schedule != nil && d.Schedule.Cron != "" {
		errs = append(errs, errors.New("after non ammesso con schedule.cron"))
	}
	if _, err := d.AfterWait(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// AfterWait returns the maximum wait for the dependencies.
func (d *Descriptor) AfterWait() (time.Duration, error) {
	if d.AfterTimeout == "" {
		return DefaultAfterTimeout, nil
	}
	t, err := time.ParseDuration(d.AfterTimeout)
	if err != nil || t < time.Minute || t > maxAfterTimeout {
		return 0, fmt.Errorf("afterTimeout %q non valido: durata tra 1m e %s", d.AfterTimeout, maxAfterTimeout)
	}
	return t, nil
}

// maxOverride limits the length of a freeze override reason.
const maxOverride = 500

// maxStepParam limits the length of custom step parameters.
const maxStepParam = 1024

// maxCallbacks limits the callbacks of a deploy: every event makes one call per callback.
const maxCallbacks = 5

// nameRe limits IDs and schedule names, also used as directory names on the worker.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const nameRule = "ammessi lettere, cifre, '.', '_', ':', '-' (max 128, iniziale alfanumerica)"

func validName(s string) bool { return nameRe.MatchString(s) }

// validate checks that exactly one of At and Cron is set and that the cron is valid.
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

// cronParser accepts the same format as DBOS (6 fields, with seconds).
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// CronExpr returns the expression in the 6-field format DBOS requires, adding "0" as the
// seconds field to classic 5-field expressions.
func (s *Schedule) CronExpr() string {
	expr := strings.TrimSpace(s.Cron)
	if len(strings.Fields(expr)) == 5 {
		return "0 " + expr
	}
	return expr
}

// ScheduleName returns the name of the recurring schedule.
func (d *Descriptor) ScheduleName() string {
	if d.Schedule != nil && d.Schedule.Name != "" {
		return d.Schedule.Name
	}
	return d.Application + "-" + d.Environment
}

// SecretRefByName returns the reference mapped to a logical name.
func (d *Descriptor) SecretRefByName(name string) (string, bool) {
	for _, s := range d.Secrets {
		if s.Name == name {
			return s.Ref, true
		}
	}
	return "", false
}
