package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// KindRelease identifica il descrittore di una release composta.
const KindRelease = "Release"

// Politiche di una release quando un deploy fallisce.
const (
	// ReleaseOnFailureStop non avvia i deploy non ancora partiti; quelli in corso terminano.
	ReleaseOnFailureStop = "stop"
	// ReleaseOnFailureContinue prosegue con i deploy che non dipendono da quello fallito.
	ReleaseOnFailureContinue = "continue"
	// ReleaseOnFailureRollback si ferma come stop, poi riporta i deploy riusciti alla versione
	// precedente, in ordine inverso alle dipendenze.
	ReleaseOnFailureRollback = "rollback"
)

// maxReleaseDeploys limita i deploy di una release.
const maxReleaseDeploys = 50

// Release rilascia insieme più deploy con dipendenze tra loro: quelli indipendenti in
// parallelo, gli altri dopo il successo dei deploy da cui dipendono.
type Release struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	// ID identifica la release ed è la chiave di idempotenza: obbligatorio.
	ID string `json:"id" yaml:"id"`
	// OnFailure è stop (default), continue o rollback.
	OnFailure string          `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	Deploys   []ReleaseDeploy `json:"deploys" yaml:"deploys"`
}

// ReleaseDeploy è un deploy della release.
type ReleaseDeploy struct {
	// Name identifica il deploy nella release e nelle dipendenze (after).
	Name string `json:"name" yaml:"name"`
	// File è il descrittore YAML, relativo al file della release: solo per la CLI, che lo
	// legge e lo invia come Descriptor.
	File       string      `json:"file,omitempty" yaml:"file,omitempty"`
	Descriptor *Descriptor `json:"descriptor,omitempty" yaml:"descriptor,omitempty"`
	// After sono i nomi dei deploy della release che devono riuscire prima di questo.
	After []string `json:"after,omitempty" yaml:"after,omitempty"`
}

// ParseReleaseJSON legge una release JSON (API): i descrittori devono essere inclusi.
func ParseReleaseJSON(data []byte) (*Release, error) {
	var r Release
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("release JSON non valida: %w", err)
	}
	return &r, r.Validate()
}

// LoadReleaseYAMLFile legge una release YAML e i descrittori a cui rimanda (file), con
// percorsi relativi alla directory della release.
func LoadReleaseYAMLFile(path string) (*Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Release
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("release YAML non valida: %w", err)
	}
	for i := range r.Deploys {
		rd := &r.Deploys[i]
		if rd.File == "" {
			continue
		}
		if rd.Descriptor != nil {
			return nil, fmt.Errorf("deploys[%d]: file e descriptor sono alternativi", i)
		}
		f := rd.File
		if !filepath.IsAbs(f) {
			f = filepath.Join(filepath.Dir(path), f)
		}
		d, err := LoadYAMLFile(f)
		if err != nil {
			return nil, fmt.Errorf("deploys[%d] (%s): %w", i, rd.File, err)
		}
		rd.Descriptor, rd.File = d, ""
	}
	return &r, r.Validate()
}

// Validate controlla la release: nomi e ID distinti, dipendenze esistenti e senza cicli,
// descrittori validi e senza schedule né dry-run.
func (r *Release) Validate() error {
	var errs []error
	if r.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion deve essere %q", APIVersion))
	}
	if r.Kind != KindRelease {
		errs = append(errs, fmt.Errorf("kind deve essere %q", KindRelease))
	}
	if !validName(r.ID) {
		errs = append(errs, fmt.Errorf("id %q non valido (obbligatorio): %s", r.ID, nameRule))
	}
	if !slices.Contains([]string{"", ReleaseOnFailureStop, ReleaseOnFailureContinue, ReleaseOnFailureRollback}, r.OnFailure) {
		errs = append(errs, fmt.Errorf("onFailure %q non valido: stop, continue o rollback", r.OnFailure))
	}
	if len(r.Deploys) == 0 || len(r.Deploys) > maxReleaseDeploys {
		errs = append(errs, fmt.Errorf("deploys: da 1 a %d", maxReleaseDeploys))
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for i, rd := range r.Deploys {
		if !validName(rd.Name) || names[rd.Name] {
			errs = append(errs, fmt.Errorf("deploys[%d]: name %q non valido o duplicato", i, rd.Name))
		}
		names[rd.Name] = true
		if rd.File != "" {
			errs = append(errs, fmt.Errorf("deploys[%d]: file non risolto, indicare descriptor", i))
			continue
		}
		d := rd.Descriptor
		if d == nil {
			errs = append(errs, fmt.Errorf("deploys[%d]: descriptor obbligatorio", i))
			continue
		}
		if err := d.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("deploys[%d] (%s): %w", i, rd.Name, err))
		}
		if d.Schedule != nil || d.DryRun {
			errs = append(errs, fmt.Errorf("deploys[%d] (%s): schedule e dryRun non ammessi in una release", i, rd.Name))
		}
		id := r.DeploymentID(rd)
		if !validName(id) || ids[id] {
			errs = append(errs, fmt.Errorf("deploys[%d]: id del deploy %q non valido o duplicato", i, id))
		}
		ids[id] = true
	}
	for i, rd := range r.Deploys {
		for _, a := range rd.After {
			if !names[a] || a == rd.Name {
				errs = append(errs, fmt.Errorf("deploys[%d] (%s): after %q non è un altro deploy della release", i, rd.Name, a))
			}
		}
	}
	if len(errs) == 0 {
		if _, err := r.Order(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeploymentID è l'ID del deploy nella release: quello del descrittore o "<release>.<nome>".
func (r *Release) DeploymentID(rd ReleaseDeploy) string {
	if rd.Descriptor != nil && rd.Descriptor.ID != "" {
		return rd.Descriptor.ID
	}
	return r.ID + "." + rd.Name
}

// FailurePolicy restituisce onFailure con il default.
func (r *Release) FailurePolicy() string {
	if r.OnFailure == "" {
		return ReleaseOnFailureStop
	}
	return r.OnFailure
}

// Order restituisce i deploy in ordine topologico: ognuno dopo quelli da cui dipende, a parità
// nell'ordine del file. Errore se le dipendenze formano un ciclo.
func (r *Release) Order() ([]ReleaseDeploy, error) {
	done := map[string]bool{}
	out := make([]ReleaseDeploy, 0, len(r.Deploys))
	for len(out) < len(r.Deploys) {
		progress := false
		for _, rd := range r.Deploys {
			if done[rd.Name] {
				continue
			}
			ready := true
			for _, a := range rd.After {
				ready = ready && done[a]
			}
			if ready {
				done[rd.Name], progress = true, true
				out = append(out, rd)
			}
		}
		if !progress {
			var cycle []string
			for _, rd := range r.Deploys {
				if !done[rd.Name] {
					cycle = append(cycle, rd.Name)
				}
			}
			return nil, fmt.Errorf("deploys: dipendenze circolari tra %v", cycle)
		}
	}
	return out, nil
}
