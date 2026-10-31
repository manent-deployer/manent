package descriptor

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseYAMLEsempio(t *testing.T) {
	data, err := os.ReadFile("../../examples/descriptor.yml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Deploy.Plugin != "remote-copy" || len(d.Deploy.Targets) != 1 {
		t.Errorf("descrittore inatteso: %+v", d.Deploy)
	}
}

func TestParseJSONEsempio(t *testing.T) {
	data, err := os.ReadFile("../../examples/descriptor.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJSON(data); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrori(t *testing.T) {
	tests := map[string]string{
		"campo sconosciuto": "apiVersion: manent/v1\nfoo: bar\n",
		"campi obbligatori": "apiVersion: manent/v1\n",
		"apiVersion errata": "apiVersion: v0\napplication: a\n",
		"segreto duplicato": "apiVersion: manent/v1\nsecrets:\n  - {name: a, ref: env:A}\n  - {name: a, ref: env:B}\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseYAML([]byte(in)); err == nil {
				t.Fatal("atteso errore")
			}
		})
	}
}

func TestSchedule(t *testing.T) {
	base := "apiVersion: manent/v1\napplication: app\nenvironment: prod\n" +
		"artifact: {type: application, name: app, source: ./app.tgz}\n" +
		"deploy: {targets: [{host: h1}]}\n"
	tests := []struct {
		name     string
		yaml     string
		wantErr  bool
		wantCron string
	}{
		{name: "at", yaml: "schedule: {at: 2030-01-01T02:00:00+01:00}\n"},
		{name: "cron a 5 campi", yaml: "schedule: {cron: \"0 2 * * *\"}\n", wantCron: "0 0 2 * * *"},
		{name: "cron a 6 campi con timezone", yaml: "schedule: {cron: \"30 0 2 * * 1-5\", timezone: Europe/Rome}\n", wantCron: "30 0 2 * * 1-5"},
		{name: "descrittore cron", yaml: "schedule: {cron: \"@daily\"}\n", wantCron: "@daily"},
		{name: "vuota", yaml: "schedule: {}\n", wantErr: true},
		{name: "at e cron insieme", yaml: "schedule: {at: 2030-01-01T02:00:00Z, cron: \"0 2 * * *\"}\n", wantErr: true},
		{name: "cron non valido", yaml: "schedule: {cron: \"61 * * * *\"}\n", wantErr: true},
		{name: "timezone non valida", yaml: "schedule: {cron: \"0 2 * * *\", timezone: Marte/Base}\n", wantErr: true},
		{name: "id con cron", yaml: "id: x\nschedule: {cron: \"0 2 * * *\"}\n", wantErr: true},
		{name: "name con at", yaml: "schedule: {at: 2030-01-01T02:00:00Z, name: n}\n", wantErr: true},
		{name: "nome schedule con slash", yaml: "schedule: {cron: \"@daily\", name: \"../x\"}\n", wantErr: true},
		{name: "id con path traversal", yaml: "id: \"../../etc\"\nschedule: {at: 2030-01-01T02:00:00Z}\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseYAML([]byte(base + tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatal("atteso errore")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantCron != "" && d.Schedule.CronExpr() != tt.wantCron {
				t.Errorf("CronExpr = %q, atteso %q", d.Schedule.CronExpr(), tt.wantCron)
			}
			if d.ScheduleName() != "app-app-prod" {
				t.Errorf("nome schedule di default inatteso: %q", d.ScheduleName())
			}
		})
	}
}

func TestParseYAMLEsempioSchedulato(t *testing.T) {
	data, err := os.ReadFile("../../examples/descriptor-scheduled.yml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.ScheduleName() != "myapp-nightly" || d.Schedule.CronExpr() != "0 0 2 * * *" {
		t.Errorf("schedule inattesa: %+v", d.Schedule)
	}
}

func TestDryRunConSchedule(t *testing.T) {
	base := "apiVersion: manent/v1\napplication: app\nenvironment: prod\n" +
		"artifact: {type: application, name: app, source: ./app.tgz}\ndryRun: true\n"
	if _, err := ParseYAML([]byte(base)); err != nil {
		t.Fatalf("dry-run senza schedule deve essere valido: %v", err)
	}
	if _, err := ParseYAML([]byte(base + "schedule: {at: 2030-01-01T02:00:00+01:00}\n")); err == nil {
		t.Fatal("atteso errore: dryRun con schedule")
	}
}

func TestCallbacks(t *testing.T) {
	base := "apiVersion: manent/v1\napplication: app\nenvironment: prod\n" +
		"artifact: {type: application, name: app, source: ./app.tgz}\n"
	tests := map[string]struct {
		yaml    string
		wantErr bool
	}{
		"senza auth":        {yaml: "callbacks: [{url: https://ci.example.com/hook}]\n"},
		"bearer con eventi": {yaml: "secrets: [{name: tok, ref: \"env:TOK\"}]\ncallbacks: [{url: https://ci.example.com/hook, events: [step], auth: {type: bearer, secret: tok}}]\n"},
		// The secrets of the environment configuration must never leave through a callback.
		"segreto non dichiarato": {yaml: "callbacks: [{url: https://ci.example.com/hook, auth: {type: bearer, secret: kube-token}}]\n", wantErr: true},
		"url relativo":           {yaml: "callbacks: [{url: /hook}]\n", wantErr: true},
		"schema non http":        {yaml: "callbacks: [{url: \"file:///etc/passwd\"}]\n", wantErr: true},
		"credenziali in url":     {yaml: "callbacks: [{url: \"https://u:p@ci.example.com/\"}]\n", wantErr: true},
		"evento sconosciuto":     {yaml: "callbacks: [{url: https://ci.example.com, events: [tutto]}]\n", wantErr: true},
		"auth senza secret":      {yaml: "callbacks: [{url: https://ci.example.com, auth: {type: bearer}}]\n", wantErr: true},
		"basic senza user":       {yaml: "callbacks: [{url: https://ci.example.com, auth: {type: basic, secret: tok}}]\n", wantErr: true},
		"header senza nome":      {yaml: "callbacks: [{url: https://ci.example.com, auth: {type: header, secret: tok}}]\n", wantErr: true},
		"tipo sconosciuto":       {yaml: "callbacks: [{url: https://ci.example.com, auth: {type: oauth, secret: tok}}]\n", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseYAML([]byte(base + tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("errore %v, atteso errore=%v", err, tt.wantErr)
			}
		})
	}
}

func TestRollback(t *testing.T) {
	base := "apiVersion: manent/v1\napplication: app\nenvironment: prod\n" +
		"artifact: {type: application, name: app, source: ./app.tgz}\n"
	d, err := ParseYAML([]byte(base + "rollback: {enabled: true, on: [deploy, post-test], locator: cmdb}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Rollback.IsEnabled() || !d.Rollback.Triggers("post-test") || d.Rollback.Triggers("activation") {
		t.Errorf("rollback inatteso: %+v", d.Rollback)
	}
	for _, bad := range []string{
		"rollback: {on: [deploy, deploy]}\n",
		"rollback: {on: [\"\"]}\n",
		"rollback: {locator: \"../x\"}\n",
		"rollback: {mode: auto}\n",
	} {
		if _, err := ParseYAML([]byte(base + bad)); err == nil {
			t.Errorf("atteso errore per %q", bad)
		}
	}
	var nilRB *Rollback
	if nilRB.IsEnabled() || nilRB.Triggers("deploy") {
		t.Error("un rollback assente non deve essere attivo")
	}
}

func TestDipendenze(t *testing.T) {
	d, err := LoadYAMLFile("../../examples/descriptor-after.yml")
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := d.AfterWait(); len(d.After) != 1 || w != 2*time.Hour {
		t.Fatalf("dipendenze inattese: %v %v", d.After, w)
	}
	cases := map[string]func(*Descriptor){
		"se stesso":       func(d *Descriptor) { d.After = []string{d.ID} },
		"duplicato":       func(d *Descriptor) { d.After = []string{"a", "a"} },
		"nome non valido": func(d *Descriptor) { d.After = []string{"a b"} },
		"dry-run":         func(d *Descriptor) { d.DryRun = true },
		"timeout":         func(d *Descriptor) { d.AfterTimeout = "10s" },
		"senza after":     func(d *Descriptor) { d.After = nil },
		"cron": func(d *Descriptor) {
			d.ID, d.Schedule = "", &Schedule{Cron: "0 2 * * *"}
		},
	}
	for name, mut := range cases {
		c := *d
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
	if w, _ := (&Descriptor{After: []string{"a"}}).AfterWait(); w != DefaultAfterTimeout {
		t.Errorf("timeout di default: %v", w)
	}
}

func TestRelease(t *testing.T) {
	r, err := LoadReleaseYAMLFile("../../examples/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	order, _ := r.Order()
	var names []string
	for _, rd := range order {
		names = append(names, rd.Name)
	}
	if strings.Join(names, " ") != "db api worker web" || r.DeploymentID(order[0]) != "shop-2026-10.db" || r.FailurePolicy() != ReleaseOnFailureRollback {
		t.Fatalf("release inattesa: %v %s", names, r.FailurePolicy())
	}
	if order[0].Descriptor == nil || order[0].Descriptor.Artifact.Type != "migration" || order[0].File != "" {
		t.Errorf("descrittore non caricato: %+v", order[0])
	}
	cases := map[string]func(*Release){
		"ciclo":             func(r *Release) { r.Deploys[0].After = []string{"web"} },
		"after sconosciuto": func(r *Release) { r.Deploys[1].After = []string{"nessuno"} },
		"nome duplicato":    func(r *Release) { r.Deploys[1].Name = "db" },
		"id duplicato": func(r *Release) {
			a, b := *r.Deploys[0].Descriptor, *r.Deploys[1].Descriptor
			a.ID, b.ID = "x", "x"
			r.Deploys[0].Descriptor, r.Deploys[1].Descriptor = &a, &b
		},
		"schedule": func(r *Release) {
			d := *r.Deploys[0].Descriptor
			at := time.Now().Add(time.Hour)
			d.Schedule = &Schedule{At: &at}
			r.Deploys[0].Descriptor = &d
		},
		"onFailure": func(r *Release) { r.OnFailure = "ignora" },
		"senza id":  func(r *Release) { r.ID = "" },
		"kind":      func(r *Release) { r.Kind = "Deploy" },
	}
	for name, mut := range cases {
		c := *r
		c.Deploys = append([]ReleaseDeploy(nil), r.Deploys...)
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
	if _, err := ParseReleaseJSON([]byte(`{"apiVersion":"manent/v1","kind":"Release","id":"r","deploys":[{"name":"a","file":"a.yml"}]}`)); err == nil {
		t.Error("file non ammesso via API")
	}
}

// artifact.name identifies the release: it must be a valid name.
func TestNomeArtefatto(t *testing.T) {
	d := Descriptor{APIVersion: APIVersion, Application: "app", Environment: "prod",
		Artifact: Artifact{Type: "application", Name: "team/api", Version: "1", Source: "/srv/a.tgz"}}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "artifact.name") {
		t.Errorf("atteso il rifiuto di artifact.name, ottenuto %v", err)
	}
}
