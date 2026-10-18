package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/governance"
)

// examplePolicy is the policy of the design document.
const examplePolicy = `package manent.governance

import rego.v1

import data.manent.lib

prod if lib.match_any(["prod", "prod-*"], input.environment)

migration if input.artifact.type == "migration"

hotfix if endswith(input.artifact.version, "-hotfix")

requires contains "preprod" if {
	prod
	input.deployment.kind != "rollback"
}

default approvals := null

approvals := {"required": 2, "timeout": "72h", "approvers": {"groups": ["dba"]}} if {
	prod
	migration
} else := {"required": 1, "timeout": "72h"} if prod

default change := null

change := {"approval": true, "timeout": "24h"} if {
	prod
	not lib.business_hours(input.now, "Europe/Rome")
}

freeze_exempt contains "weekend" if {
	hotfix
	startswith(input.deployment.freeze_override, "INC-")
}

deny contains {"code": "downgrade", "message": "downgrades in production go through a manual rollback"} if {
	prod
	input.deployment.kind == "deploy"
	lib.semver_less(input.artifact.version, input.current.version)
}

decision := {
	"rule": "custom",
	"deny": deny,
	"requires": requires,
	"approvals": approvals,
	"change": change,
	"freeze_exempt": freeze_exempt,
}
`

func input(env, kind, version string) governance.Input {
	return governance.Input{
		Phase: governance.PhaseSubmit, Now: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC),
		Application: "shop", Environment: env,
		Deployment:  governance.InputDeployment{Kind: kind},
		Artifact:    governance.InputArtifact{Name: "shop", Version: version, Type: "application"},
		RequestedBy: governance.InputIdentity{ID: "oidc:alice"},
	}
}

func decide(t *testing.T, c *Compiled, in governance.Input, windows ...governance.Window) governance.Decision {
	t.Helper()
	d, err := governance.Decide(context.Background(), in, windows, c.Evaluate)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPolicyDiEsempio(t *testing.T) {
	c, _, err := Compile(context.Background(), Files{"governance.rego": examplePolicy}, "", CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := decide(t, c, input("prod", "deploy", "1.4.0"))
	if d.Rule != "custom" || len(d.Requires) != 1 || d.Approvals.RequiredOrZero() != 1 || d.Approvals.Timeout != 72*time.Hour || d.Change != nil {
		t.Errorf("decisione in orario: %+v", d)
	}
	night := input("prod", "deploy", "1.4.0")
	night.Now = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
	night.Artifact.Type = "migration"
	d = decide(t, c, night)
	if d.Approvals.RequiredOrZero() != 2 || d.Approvals.Approvers == nil || d.Approvals.Approvers.Groups[0] != "dba" || d.Change == nil || !d.Change.Approval || d.Change.Timeout != 24*time.Hour {
		t.Errorf("migrazione fuori orario: %+v %+v", d.Approvals, d.Change)
	}
	rb := input("prod", "rollback", "1.0.0")
	if d := decide(t, c, rb); len(d.Requires) != 0 {
		t.Errorf("il rollback non richiede la promozione: %+v", d)
	}
	down := input("prod", "deploy", "1.2.0")
	down.Current = &governance.InputRelease{Version: "1.10.0"}
	if d := decide(t, c, down); len(d.Deny) != 1 || d.Deny[0].Code != "downgrade" || d.Denied() == nil {
		t.Errorf("downgrade atteso negato: %+v", d.Deny)
	}
	if d := decide(t, c, input("test", "deploy", "1.0.0")); d.Approvals != nil || len(d.Requires) != 0 {
		t.Errorf("nessun vincolo in test: %+v", d)
	}
	// Freeze: the recurring weekend window is exempted for hotfixes with an incident, year-end
	// is hard and blocks even with an override.
	weekend := governance.Window{Name: "weekend", Applications: []string{"*"}, Environments: []string{"prod"},
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	hard := weekend
	hard.Name, hard.Hard = "year-end", true
	if errs := governance.PrepareWindows([]governance.Window{weekend, hard}, "t"); len(errs) > 0 {
		t.Fatal(errs)
	}
	hf := input("prod", "deploy", "1.4.1-hotfix")
	if d := decide(t, c, hf, weekend); d.Freeze == nil || d.Freeze.Name != "weekend" {
		t.Errorf("senza incident il freeze blocca: %+v", d.Freeze)
	}
	hf.Deployment.FreezeOverride = "INC-881"
	if d := decide(t, c, hf, weekend); d.Freeze != nil {
		t.Errorf("hotfix con incident esentato: %+v", d.Freeze)
	}
	if d := decide(t, c, hf, weekend, hard); d.Freeze == nil || d.Freeze.Name != "year-end" {
		t.Errorf("il freeze hard blocca sempre: %+v", d.Freeze)
	}
}

func TestPolicyPredefinitaUgualeAlleRegoleYAML(t *testing.T) {
	rules := []governance.Rule{
		{Name: "pagamenti", Applications: []string{"pay-*"}, Environments: []string{"prod", "prod-*"},
			Approvals: governance.Approvals{Required: 2, Timeout: 24 * time.Hour, Approvers: &governance.Approvers{Groups: []string{"sre"}}}},
		{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Requires: []string{"preprod"},
			Approvals: governance.Approvals{Required: 1, When: governance.ApprovalsRollout}, Change: &governance.Change{Approval: true, Timeout: 48 * time.Hour}},
		{Name: "staging", Applications: []string{"*"}, Environments: []string{"staging"}, Change: &governance.Change{Timeout: 72 * time.Hour}},
	}
	builtin, err := governance.New(rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := Compile(context.Background(), Files{}, "default", CompileOptions{Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"pay-api", "shop", "x"} {
		for _, env := range []string{"prod", "prod-eu", "staging", "test"} {
			in := input(env, "deploy", "1.0.0")
			in.Application = app
			want, err := builtin.Evaluate(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			got := decide(t, c, in)
			if summary(got) != summary(want) {
				t.Errorf("%s/%s: opa %s, builtin %s", app, env, summary(got), summary(want))
			}
		}
	}
}

// summary describes rule and obligations of a decision.
func summary(d governance.Decision) string {
	s := d.Rule + " requires=" + strings.Join(d.Requires, ",")
	if a := d.Approvals; a != nil {
		s += " approvals=" + a.Timeout.String() + "/" + a.When
		s += strings.Repeat("+", a.Required)
		if a.AllowSelf {
			s += " self"
		}
		if a.Approvers != nil {
			s += " approvers=" + strings.Join(a.Approvers.Groups, ",")
		}
	}
	if c := d.Change; c != nil {
		s += " change=" + c.Timeout.String()
		if c.Approval {
			s += "+approval"
		}
	}
	return s
}

func TestPolicyRifiutate(t *testing.T) {
	cases := map[string]struct {
		files Files
		want  string
	}{
		"orologio":   {Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {\"notes\": [sprintf(\"%d\", [time.now_ns()])]}\n"}, "time.now_ns"},
		"rete":       {Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {\"notes\": [http.send({\"method\": \"get\", \"url\": \"http://x\"}).body]}\n"}, "http.send"},
		"indefinita": {Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {} if input.never\n"}, "undefined"},
		"schema":     {Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {\"approve\": 1}\n"}, "unknown field"},
		"riservato":  {Files{"p.rego": "package manent.lib.mine\nimport rego.v1\nx := 1\n"}, "reserved"},
		"percorso":   {Files{"../p.rego": "package x"}, "invalid file path"},
		"estensione": {Files{"p.sh": "rm -rf /"}, "not allowed"},
		"test fallito": {Files{
			"p.rego":      "package manent.governance\nimport rego.v1\ndecision := {\"approvals\": {\"required\": 1}}\n",
			"p_test.rego": "package manent.governance_test\nimport rego.v1\nimport data.manent.governance\ntest_approvals if governance.decision.approvals.required == 2\n",
		}, "tests failed"},
	}
	for name, tc := range cases {
		_, _, err := Compile(context.Background(), tc.files, "", CompileOptions{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: errore atteso con %q, ottenuto %v", name, tc.want, err)
		}
	}
	if _, _, err := Compile(context.Background(), Files{"p.rego": "package manent.governance\nimport rego.v1\ndecision := {}\n"}, "", CompileOptions{RequireTests: true}); err == nil || !strings.Contains(err.Error(), "no tests") {
		t.Errorf("test obbligatori: %v", err)
	}
}

func TestTestDellaRevisione(t *testing.T) {
	files := Files{
		"governance.rego": examplePolicy,
		"governance_test.rego": `package manent.governance_test

import rego.v1

import data.manent.governance

test_prod_one_approval if {
	d := governance.decision with input as {"application": "shop", "environment": "prod", "now": "2026-10-19T10:00:00Z", "deployment": {"kind": "deploy"}, "artifact": {"version": "1.0.0", "type": "application"}}
	d.approvals.required == 1
}

test_rollback_without_promotion if {
	d := governance.decision with input as {"application": "shop", "environment": "prod", "now": "2026-10-19T10:00:00Z", "deployment": {"kind": "rollback"}, "artifact": {"version": "1.0.0"}}
	count(d.requires) == 0
}
`,
		"data.yaml": "manent:\n  freezes:\n    - name: natale\n      applications: ['*']\n      environments: [prod]\n      from: 2026-12-20T00:00:00+01:00\n      to: 2027-01-07T00:00:00+01:00\n      hard: true\n",
	}
	c, tests, err := Compile(context.Background(), files, "", CompileOptions{RequireTests: true})
	if err != nil {
		t.Fatalf("%v %+v", err, tests)
	}
	if len(tests) != 2 || !tests[0].Pass || !tests[1].Pass {
		t.Errorf("test inattesi: %+v", tests)
	}
	if len(c.Windows) != 1 || !c.Windows[0].Hard {
		t.Errorf("finestre della revisione: %+v", c.Windows)
	}
}

func TestConfrontoVersioni(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.10.0", -1}, {"v1.2", "1.2.0", 0}, {"1.2.0-rc1", "1.2.0", -1}, {"2.0", "1.9.9", 1}, {"1.0.0-rc.2", "1.0.0-rc.10", -1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("%s vs %s: %d, atteso %d", tc.a, tc.b, got, tc.want)
		}
	}
}
