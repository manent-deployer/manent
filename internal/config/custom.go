package config

import (
	"fmt"
	"regexp"
	"strings"
)

// CustomSteps configura l'esecuzione degli step personalizzati.
type CustomSteps struct {
	// SpecKeyRefs sono i riferimenti alle chiavi HMAC che firmano le specifiche fotografate
	// nei piani: la prima firma, tutte verificano (rotazione). Senza, le specifiche non sono
	// firmate e chi scrive nel database DBOS può far eseguire comandi sui target.
	SpecKeyRefs []string `yaml:"spec_key_refs"`
	// RevokedSpecs sono gli sha256 delle specifiche da non eseguire più, anche se già
	// fotografate in deploy accodati o schedule.
	RevokedSpecs []string `yaml:"revoked_specs"`
	// KnownHosts è il file known_hosts per i comandi via SSH (default ~/.ssh/known_hosts).
	KnownHosts string `yaml:"known_hosts"`
	// InsecureIgnoreHostKey disattiva la verifica della host key: solo per lo sviluppo.
	InsecureIgnoreHostKey bool `yaml:"insecure_ignore_host_key"`
	// SecretsDir è la directory dei segreti sui target, su tmpfs (default /dev/shm/manent).
	SecretsDir string `yaml:"secrets_dir"`
	// AllowHTTP ammette URL http negli step http (default: solo https).
	AllowHTTP bool `yaml:"allow_http"`
}

// DefaultCustomSecretsDir è la directory dei segreti sui target.
const DefaultCustomSecretsDir = "/dev/shm/manent"

var (
	specHashRe   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	remoteDirRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	standardStep = []string{"secrets", "artifacts", "qa", "deploy", "post-test", "activation"}
)

// HasCustom indica se la pipeline contiene step personalizzati.
func (p Pipeline) HasCustom() bool {
	for _, s := range p.Steps {
		if s.IsCustom() {
			return true
		}
	}
	return false
}

func (p *Pipeline) applyDefaults() {
	if p.Custom.SecretsDir == "" {
		p.Custom.SecretsDir = DefaultCustomSecretsDir
	}
	for i := range p.Steps {
		p.Steps[i].ApplyDefaults()
	}
}

func (p Pipeline) validateCustom() []error {
	var errs []error
	for _, s := range p.Steps {
		if s.IsCustom() {
			for _, std := range standardStep {
				if s.Name == std {
					errs = append(errs, fmt.Errorf("step %q: il nome è di uno step standard", s.Name))
				}
			}
		}
		if err := s.Spec.Validate(s.Name, p.Custom.AllowHTTP); err != nil {
			errs = append(errs, fmt.Errorf("pipeline.steps: %w", err))
		}
	}
	for _, h := range p.Custom.RevokedSpecs {
		if !specHashRe.MatchString(h) {
			errs = append(errs, fmt.Errorf("pipeline.custom.revoked_specs: %q non è uno sha256 esadecimale minuscolo", h))
		}
	}
	if d := p.Custom.SecretsDir; d != "" && (!remoteDirRe.MatchString(d) || strings.Contains(d, "..")) {
		errs = append(errs, fmt.Errorf("pipeline.custom.secrets_dir %q non valida: percorso assoluto senza '..'", d))
	}
	return errs
}

func (p Pipeline) securityWarnings() []string {
	if !p.HasCustom() {
		return nil
	}
	var w []string
	if len(p.Custom.SpecKeyRefs) == 0 {
		w = append(w, "pipeline.custom.spec_key_refs: le specifiche degli step personalizzati non sono firmate (chi scrive nel database DBOS può far eseguire comandi sui target)")
	}
	if p.Custom.InsecureIgnoreHostKey {
		w = append(w, "pipeline.custom.insecure_ignore_host_key: i comandi via SSH non verificano la host key dei target")
	}
	return w
}
