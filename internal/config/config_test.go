package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestIntegrazioni(t *testing.T) {
	cfg, err := load(t, `
integrations:
  public_url: https://manent.example.com
  language: it
  slack:
    bot_token_ref: "env:SLACK_BOT_TOKEN"
    signing_secret_ref: "env:SLACK_SIGNING_SECRET"
    identities:
      U024BE7LH: {subject: "oidc:alice", groups: [cab]}
  notifications:
    - {name: prod, applications: ["*"], environments: [prod], slack: {channel: C0123}}
    - {name: teams-cab, applications: ["*"], environments: ["prod*"], events: [approval], teams: {webhook_ref: "env:TEAMS_WEBHOOK"}}
  change:
    provider: servicenow
    servicenow: {url: "https://acme.service-now.com", username: manent, password_ref: "env:SN_PASSWORD"}
governance:
  rules:
    - {name: prod, applications: ["*"], environments: [prod], change: {approval: true}}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Integrations.Notifications) != 2 || cfg.Governance.Rules[0].Change == nil || !cfg.Governance.Rules[0].Change.Approval {
		t.Errorf("integrazioni: %+v", cfg.Integrations)
	}
	for in, want := range map[string]string{
		"integrations: {notifications: [{name: Prod, applications: ['*'], environments: ['*'], teams: {webhook_ref: x}}]}\n":                                                                                                    "name",
		"integrations: {notifications: [{name: a, applications: ['*'], environments: ['*']}]}\n":                                                                                                                                "slack oppure teams",
		"integrations: {notifications: [{name: a, applications: ['*'], environments: ['*'], slack: {channel: C1}}]}\n":                                                                                                          "bot_token_ref",
		"integrations: {notifications: [{name: a, applications: ['*'], environments: ['*'], events: [deploy], teams: {webhook_ref: x}}]}\n":                                                                                     "events",
		"integrations: {slack: {bot_token_ref: x, identities: {U1: {subject: alice}}}}\n":                                                                                                                                       "U1",
		"governance: {rules: [{applications: ['*'], environments: [prod], change: {}}]}\n":                                                                                                                                      "integrations.change.provider",
		"integrations: {change: {provider: jira, jira: {url: 'https://jira', token_ref: x, project: CHG, issue_type: Change}}}\ngovernance: {rules: [{applications: ['*'], environments: [prod], change: {approval: true}}]}\n": "approved_statuses",
		"integrations: {change: {provider: remedy}}\n":                                                                                                                                                                          "remedy",
	} {
		if _, err := load(t, in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: errore %v, atteso %q", in, err, want)
		}
	}
}

func TestStepPersonalizzati(t *testing.T) {
	cfg, err := load(t, `
pipeline:
  custom:
    spec_key_refs: ["env:MANENT_SPEC_KEY"]
    revoked_specs: ["`+strings.Repeat("ab", 32)+`"]
  steps:
    - {name: secrets, mode: mandatory}
    - {name: artifacts, mode: mandatory}
    - name: db-migrate
      mode: optional
      type: command
      timeout: 15m
      secrets: [db-admin]
      params:
        database: {pattern: "[a-z][a-z0-9_]{0,62}", required: true}
      outputs:
        applied: {pattern: "[0-9]{1,6}"}
      ssh: {user: migrator, key_secret: ssh-migrate}
      run:
        run_once: true
        command: ["/opt/tools/migrate", "up", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"]
      dry_run:
        command: ["/opt/tools/migrate", "plan", "--to", "{{ .Version }}"]
        secrets: [db-readonly]
    - {name: deploy, mode: mandatory}
    - name: cdn-purge
      mode: optional
      enabled: true
      type: http
      secrets: [cdn-token]
      run:
        url: "https://api.cdn.example.com/zones/{{ .Params.zone }}/purge"
        auth: {type: bearer, secret: cdn-token}
        json: {tags: ["{{ .Application }}"], purge_everything: false}
      params:
        zone: {pattern: "[a-f0-9]{32}", required: true}
`)
	if err != nil {
		t.Fatal(err)
	}
	steps := cfg.Pipeline.Steps
	m, c := steps[2], steps[4]
	if m.Type != "command" || m.Timeout != 15*time.Minute || m.Run.On != "targets" || m.DryRun.On != "targets" || !m.DryRun.RunOnce || m.SSH.User != "migrator" {
		t.Errorf("step command: %+v", m.Spec)
	}
	if c.Run.Method != "POST" || c.Timeout != 10*time.Minute || c.Run.JSON.(map[string]any)["purge_everything"] != false {
		t.Errorf("step http: %+v", c.Run)
	}
	if !cfg.Pipeline.HasCustom() || cfg.Pipeline.Custom.SecretsDir != DefaultCustomSecretsDir {
		t.Errorf("pipeline.custom: %+v", cfg.Pipeline.Custom)
	}
	for _, w := range cfg.SecurityWarnings() {
		if strings.Contains(w, "spec_key_refs") {
			t.Errorf("avviso inatteso con le chiavi configurate: %s", w)
		}
	}

	_, err = load(t, `
pipeline:
  custom:
    revoked_specs: ["abc"]
    secrets_dir: "/dev/shm/../etc"
  steps:
    - {name: deploy, mode: mandatory, type: command, run: {command: ["/bin/true"]}, ssh: {key_secret: k}}
    - {name: hook, mode: optional, type: http, run: {url: "http://example.com/x"}}
    - {name: qa, mode: optional, timeout: 1m}
`)
	for _, want := range []string{`step "deploy": il nome è di uno step standard`, `schema "http" non ammesso`, "richiedono type", "revoked_specs", "secrets_dir"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso errore %q, ricevuto %v", want, err)
		}
	}

	cfg, err = load(t, `
pipeline:
  custom: {insecure_ignore_host_key: true}
  steps:
    - {name: deploy, mode: mandatory}
    - {name: hook, mode: optional, type: http, run: {url: "https://example.com/x"}}
`)
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(cfg.SecurityWarnings(), "\n")
	if !strings.Contains(w, "spec_key_refs") || !strings.Contains(w, "insecure_ignore_host_key") {
		t.Errorf("avvisi: %s", w)
	}
}
