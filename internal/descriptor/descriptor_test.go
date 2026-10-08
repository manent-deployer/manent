package descriptor

import (
	"os"
	"testing"
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
		"campo sconosciuto": "apiVersion: go-dart/v1\nfoo: bar\n",
		"campi obbligatori": "apiVersion: go-dart/v1\n",
		"apiVersion errata": "apiVersion: v0\napplication: a\n",
		"segreto duplicato": "apiVersion: go-dart/v1\nsecrets:\n  - {name: a, ref: env:A}\n  - {name: a, ref: env:B}\n",
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
	base := "apiVersion: go-dart/v1\napplication: app\nenvironment: prod\n" +
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
			if d.ScheduleName() != "app-prod" {
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
