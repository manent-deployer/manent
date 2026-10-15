// Package envconfig recupera da un sistema di gestione delle configurazioni (es. Consul)
// la configurazione di deploy di un'applicazione in un ambiente e la fonde con il descrittore.
package envconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// ErrNotFound indica che per l'applicazione/ambiente non esiste configurazione.
var ErrNotFound = errors.New("configurazione di ambiente non trovata")

// EnvConfig è la configurazione di deploy di un ambiente: plugin, target e parametri del plugin.
// Non deve contenere valori segreti, solo riferimenti: viene persistita da DBOS come output di step.
type EnvConfig struct {
	Policy  string                 `json:"policy,omitempty" yaml:"policy,omitempty"`
	Plugin  string                 `json:"plugin,omitempty" yaml:"plugin,omitempty"`
	Targets []descriptor.Target    `json:"targets,omitempty" yaml:"targets,omitempty"`
	Params  map[string]string      `json:"params,omitempty" yaml:"params,omitempty"`
	Secrets []descriptor.SecretRef `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	// Rollback impone il rollback automatico dell'ambiente (es. sempre attivo in produzione),
	// nei limiti della policy del tipo di artefatto.
	Rollback *descriptor.Rollback `json:"rollback,omitempty" yaml:"rollback,omitempty"`
	// Rollout è la strategia di rilascio a ondate dell'ambiente.
	Rollout *descriptor.Rollout `json:"rollout,omitempty" yaml:"rollout,omitempty"`
	// StepParams sono i parametri degli step personalizzati, per step.
	StepParams map[string]map[string]string `json:"step_params,omitempty" yaml:"step_params,omitempty"`
	// Traffic è l'integrazione con load balancer e router: svuotamento dei target e
	// spostamento del traffico tra gli slot.
	Traffic *descriptor.TrafficConfig `json:"traffic,omitempty" yaml:"traffic,omitempty"`
}

// Document è una configurazione letta dal provider, con i metadati per l'audit.
type Document struct {
	Config EnvConfig
	// Source identifica la configurazione nel provider (es. chiave Consul).
	Source string
	// Revision è la versione della configurazione (es. ModifyIndex di Consul), se disponibile.
	Revision string
}

// Provider legge la configurazione di un'applicazione in un ambiente.
type Provider interface {
	Get(ctx context.Context, application, environment string) (*Document, error)
}

// Parse decodifica una configurazione YAML o JSON, rifiutando campi sconosciuti.
func Parse(data []byte) (EnvConfig, error) {
	var c EnvConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("configurazione di ambiente non valida: %w", err)
	}
	return c, nil
}

// expandKey sostituisce {application} e {environment} nel modello di chiave o percorso.
func expandKey(tmpl, application, environment string) string {
	return strings.NewReplacer("{application}", application, "{environment}", environment).Replace(tmpl)
}

// Merge applica la configurazione di ambiente al descrittore e restituisce il descrittore effettivo.
// La configurazione di ambiente è autorevole: il descrittore può solo aggiungere ciò che non vi è
// definito. Un valore definito in entrambi con contenuto diverso è un errore, mai una sovrascrittura.
func Merge(d descriptor.Descriptor, c EnvConfig) (descriptor.Descriptor, error) {
	var errs []error
	pick := func(field, env, desc string) string {
		if env != "" && desc != "" && env != desc {
			errs = append(errs, fmt.Errorf("%s: %q nella configurazione di ambiente, %q nel descrittore", field, env, desc))
		}
		if env != "" {
			return env
		}
		return desc
	}
	d.Deploy.Policy = pick("deploy.policy", c.Policy, d.Deploy.Policy)
	d.Deploy.Plugin = pick("deploy.plugin", c.Plugin, d.Deploy.Plugin)

	if len(c.Targets) > 0 {
		if len(d.Deploy.Targets) > 0 && !slices.EqualFunc(c.Targets, d.Deploy.Targets, descriptor.Target.Equal) {
			errs = append(errs, errors.New("deploy.targets definiti sia nella configurazione di ambiente sia nel descrittore"))
		}
		d.Deploy.Targets = slices.Clone(c.Targets)
	}

	params := maps.Clone(d.Deploy.Params)
	if params == nil && len(c.Params) > 0 {
		params = map[string]string{}
	}
	for _, k := range slices.Sorted(maps.Keys(c.Params)) {
		if v, ok := params[k]; ok && v != c.Params[k] {
			errs = append(errs, fmt.Errorf("deploy.params.%s: valore diverso tra configurazione di ambiente e descrittore", k))
		}
		params[k] = c.Params[k]
	}
	d.Deploy.Params = params

	if len(c.StepParams) > 0 {
		sp := map[string]map[string]string{}
		for step, params := range d.StepParams {
			sp[step] = maps.Clone(params)
		}
		for _, step := range slices.Sorted(maps.Keys(c.StepParams)) {
			if sp[step] == nil {
				sp[step] = map[string]string{}
			}
			for _, k := range slices.Sorted(maps.Keys(c.StepParams[step])) {
				if v, ok := sp[step][k]; ok && v != c.StepParams[step][k] {
					errs = append(errs, fmt.Errorf("stepParams.%s.%s: valore diverso tra configurazione di ambiente e descrittore", step, k))
				}
				sp[step][k] = c.StepParams[step][k]
			}
		}
		d.StepParams = sp
	}

	secrets := slices.Clone(c.Secrets)
	for _, s := range d.Secrets {
		i := slices.IndexFunc(secrets, func(e descriptor.SecretRef) bool { return e.Name == s.Name })
		switch {
		case i < 0:
			secrets = append(secrets, s)
		case secrets[i].Ref != s.Ref:
			errs = append(errs, fmt.Errorf("secrets.%s: riferimento diverso tra configurazione di ambiente e descrittore", s.Name))
		}
	}
	d.Secrets = secrets

	if c.Rollback != nil {
		rb := descriptor.Rollback{}
		if d.Rollback != nil {
			rb = *d.Rollback
		}
		if e := c.Rollback.Enabled; e != nil {
			if rb.Enabled != nil && *rb.Enabled != *e {
				errs = append(errs, fmt.Errorf("rollback.enabled: %t nella configurazione di ambiente, %t nel descrittore", *e, *rb.Enabled))
			}
			rb.Enabled = e
		}
		if len(c.Rollback.On) > 0 {
			if len(rb.On) > 0 && !slices.Equal(rb.On, c.Rollback.On) {
				errs = append(errs, errors.New("rollback.on definito sia nella configurazione di ambiente sia nel descrittore"))
			}
			rb.On = slices.Clone(c.Rollback.On)
		}
		rb.Locator = pick("rollback.locator", c.Rollback.Locator, rb.Locator)
		d.Rollback = &rb
	}

	if c.Rollout != nil {
		// La strategia dell'ambiente è autorevole: il descrittore può solo ripeterla identica.
		if d.Rollout != nil && !reflect.DeepEqual(*d.Rollout, *c.Rollout) {
			errs = append(errs, errors.New("rollout definito sia nella configurazione di ambiente sia nel descrittore"))
		}
		r := *c.Rollout
		d.Rollout = &r
	}
	// Load balancer e router sono infrastruttura: solo la configurazione di ambiente li definisce.
	if d.Traffic != nil {
		errs = append(errs, errors.New("traffic si configura solo nella configurazione di ambiente, non nel descrittore"))
	}
	d.Traffic = nil
	if c.Traffic != nil {
		if err := c.Traffic.Validate(); err != nil {
			errs = append(errs, err)
		}
		t := *c.Traffic
		d.Traffic = &t
	}
	if len(d.Deploy.Targets) == 0 {
		errs = append(errs, errors.New("nessun target di deploy: né nella configurazione di ambiente né nel descrittore"))
	}
	if err := errors.Join(errs...); err != nil {
		return d, fmt.Errorf("configurazione di ambiente incompatibile con il descrittore: %w", err)
	}
	return d, nil
}

// Resolver produce il descrittore effettivo usando il provider configurato.
type Resolver struct {
	// Provider è nil quando nessun sistema di configurazione è configurato.
	Provider Provider
	// Required fa fallire il deploy se per l'ambiente non esiste configurazione.
	Required bool
	// Policy, se presente, limita ciò che il descrittore può aggiungere.
	Policy *DescriptorPolicy
}

// Resolve legge la configurazione di ambiente e la fonde con il descrittore.
// Restituisce anche i metadati (source, revision) da registrare nel risultato del deploy.
func (r Resolver) Resolve(ctx context.Context, d descriptor.Descriptor) (descriptor.Descriptor, map[string]string, error) {
	meta := map[string]string{"source": "descriptor"}
	cfg := EnvConfig{}
	if r.Provider != nil {
		doc, err := r.Provider.Get(ctx, d.Application, d.Environment)
		switch {
		case errors.Is(err, ErrNotFound) && !r.Required:
			meta["source"] = "descriptor (configurazione di ambiente assente)"
		case err != nil:
			return d, nil, err
		default:
			cfg = doc.Config
			meta["source"] = doc.Source
			if doc.Revision != "" {
				meta["revision"] = doc.Revision
			}
		}
	}
	// Policy e fusione dipendono solo da descrittore e configurazione: un nuovo tentativo
	// darebbe lo stesso errore.
	if err := r.Policy.Check(d, cfg); err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	eff, err := Merge(d, cfg)
	if err != nil {
		return d, nil, pipeline.Permanent(err)
	}
	meta["targets"] = strconv.Itoa(len(eff.Deploy.Targets))
	return eff, meta, nil
}
