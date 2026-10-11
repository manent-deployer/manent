package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manent.yml")
	if err := os.WriteFile(p, []byte("dbos: {executor_id: w1}\n"+yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestDefaultSviluppo(t *testing.T) {
	cfg, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != AuthModeNone || cfg.DescriptorPolicy.Params["*"][0] != "*" || !*cfg.Artifacts.AllowUnlisted {
		t.Errorf("default inattesi: %+v", cfg)
	}
	if n := len(cfg.SecurityWarnings()); n != 4 {
		t.Errorf("attesi 4 avvisi di sicurezza, trovati %d", n)
	}
}

func TestConfigurazioneProduzione(t *testing.T) {
	cfg, err := load(t, `
auth:
  mode: enabled
  tokens:
    - name: ci
      sha256: "`+strings.Repeat("ab", 32)+`"
descriptor_policy:
  params:
    remote-copy: [dest_dir]
  secret_refs: ["vault:secret/data/{application}/*"]
  targets: false
artifacts:
  allow_unlisted: false
  sources:
    - prefix: https://nexus.example.com/repository/releases/
`)
	if err != nil {
		t.Fatal(err)
	}
	// La policy configurata sostituisce quella di default, non vi si aggiunge.
	if _, ok := cfg.DescriptorPolicy.Params["*"]; ok {
		t.Error("il default * non deve restare nella policy configurata")
	}
	if w := cfg.SecurityWarnings(); len(w) != 0 {
		t.Errorf("nessun avviso atteso: %v", w)
	}
}

func TestConfigurazioneSicurezzaErrata(t *testing.T) {
	for name, yaml := range map[string]string{
		"enabled senza credenziali": "auth: {mode: enabled}\n",
		"modo sconosciuto":          "auth: {mode: ldap}\n",
		"token in chiaro":           "auth: {mode: enabled, tokens: [{name: ci, sha256: s3cr3t}]}\n",
		"oidc incompleto":           "auth: {mode: enabled, oidc: {issuer: https://idp}}\n",
		"vault auth":                "secrets: {vault: {enabled: true, auth: ldap}}\n",
	} {
		if _, err := load(t, yaml); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
}
