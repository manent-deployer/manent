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

func TestDestinazioniSIEM(t *testing.T) {
	// SIEM remoto e journal locale insieme.
	cfg, err := load(t, `
siem:
  enabled: true
  network: tcp
  address: siem.example.com:6514
  destinations:
    - network: journald
    - network: unixgram
      address: /dev/log
`)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.SIEM.Targets()
	if len(got) != 3 || got[0].Network != "tcp" || got[1].Network != "journald" || got[2].Address != "/dev/log" {
		t.Errorf("destinazioni: %+v", got)
	}
	// Solo il journal: network di default (udp) senza address non è una destinazione.
	cfg, err = load(t, "siem: {enabled: true, destinations: [{network: journald}]}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SIEM.Targets(); len(got) != 1 || got[0].Network != "journald" {
		t.Errorf("solo journald: %+v", got)
	}
	if cfg, _ := load(t, "siem: {enabled: false, network: journald}\n"); len(cfg.SIEM.Targets()) != 0 {
		t.Error("SIEM disattivato con destinazioni attive")
	}
	for in, want := range map[string]string{
		"siem: {enabled: true}\n":                                     "serve address",
		"siem: {enabled: true, network: syslog, address: x}\n":        `rete "syslog" non supportata`,
		"siem: {enabled: true, destinations: [{network: tcp}]}\n":     "senza address",
		"siem: {enabled: true, network: unixgram}\n":                  "",
		"siem: {enabled: true, network: journald, address: /tmp/j}\n": "",
	} {
		_, err := load(t, in)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: errore %v, atteso %q", in, err, want)
		}
	}
}
