package config

import (
	"fmt"
	"regexp"
	"strings"
)

// CustomSteps configures the execution of custom steps.
type CustomSteps struct {
	// SpecKeyRefs reference the HMAC keys that sign the specs captured in plans: the first signs,
	// all verify (rotation). Without them specs are unsigned and whoever writes to the DBOS
	// database can run commands on the targets.
	SpecKeyRefs []string `yaml:"spec_key_refs"`
	// RevokedSpecs are the sha256 of the specs not to run any more, even if already captured in
	// enqueued deploys or schedules.
	RevokedSpecs []string `yaml:"revoked_specs"`
	// KnownHosts is the known_hosts file for commands over SSH (default ~/.ssh/known_hosts).
	KnownHosts string `yaml:"known_hosts"`
	// InsecureIgnoreHostKey disables host key verification: development only.
	InsecureIgnoreHostKey bool `yaml:"insecure_ignore_host_key"`
	// SecretsDir is the secrets directory on the targets, on tmpfs (default /dev/shm/manent).
	SecretsDir string `yaml:"secrets_dir"`
	// AllowHTTP allows http URLs in http steps (default: https only).
	AllowHTTP bool `yaml:"allow_http"`
}

// DefaultCustomSecretsDir is the secrets directory on the targets.
const DefaultCustomSecretsDir = "/dev/shm/manent"

var (
	specHashRe   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	remoteDirRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	standardStep = []string{"secrets", "artifacts", "qa", "deploy", "post-test", "activation", "drain", "restore"}
)

// HasCustom reports whether the pipeline contains custom steps.
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
					errs = append(errs, fmt.Errorf("step %q: the name belongs to a standard step", s.Name))
				}
			}
		}
		if err := s.Spec.Validate(s.Name, p.Custom.AllowHTTP); err != nil {
			errs = append(errs, fmt.Errorf("pipeline.steps: %w", err))
		}
	}
	for _, h := range p.Custom.RevokedSpecs {
		if !specHashRe.MatchString(h) {
			errs = append(errs, fmt.Errorf("pipeline.custom.revoked_specs: %q is not a lowercase hex sha256", h))
		}
	}
	if d := p.Custom.SecretsDir; d != "" && (!remoteDirRe.MatchString(d) || strings.Contains(d, "..")) {
		errs = append(errs, fmt.Errorf("invalid pipeline.custom.secrets_dir %q: absolute path without '..'", d))
	}
	return errs
}

func (p Pipeline) securityWarnings() []string {
	if !p.HasCustom() {
		return nil
	}
	var w []string
	if len(p.Custom.SpecKeyRefs) == 0 {
		w = append(w, "pipeline.custom.spec_key_refs: the specifications of custom steps are not signed (whoever writes to the DBOS database can run commands on the targets)")
	}
	if p.Custom.InsecureIgnoreHostKey {
		w = append(w, "pipeline.custom.insecure_ignore_host_key: commands over SSH do not verify the host key of the targets")
	}
	return w
}
