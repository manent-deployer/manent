package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/internal/config"
	"github.com/manent-deployer/manent/internal/customspec"
	"github.com/manent-deployer/manent/internal/descriptor"
)

// stubStep is an empty step used in tests.
type stubStep string

func (s stubStep) Name() string                               { return string(s) }
func (s stubStep) Run(context.Context, Input) (Output, error) { return nil, nil }

func newPlanner(t *testing.T) ConfigPlanner {
	t.Helper()
	reg, err := NewRegistry(stubStep("secrets"), stubStep("qa"), stubStep("deploy"), stubStep("post-test"))
	if err != nil {
		t.Fatal(err)
	}
	return ConfigPlanner{Registry: reg, Steps: []config.Step{
		{Name: "secrets", Mode: config.ModeMandatory},
		{Name: "qa", Mode: config.ModeOptional, Enabled: true},
		{Name: "deploy", Mode: config.ModeMandatory, MaxRetries: 2},
		{Name: "post-test", Mode: config.ModeOptional},
	}}
}

func names(p Plan) []string {
	out := make([]string, len(p))
	for i, s := range p {
		out[i] = s.Name
	}
	return out
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		steps   map[string]bool
		want    []string
		wantErr bool
	}{
		{name: "default da configurazione", want: []string{"secrets", "qa", "deploy"}},
		{name: "descrittore attiva e disattiva opzionali", steps: map[string]bool{"qa": false, "post-test": true}, want: []string{"secrets", "deploy", "post-test"}},
		{name: "obbligatorio non disattivabile", steps: map[string]bool{"deploy": false}, wantErr: true},
		{name: "step sconosciuto", steps: map[string]bool{"foo": true}, wantErr: true},
	}
	p := newPlanner(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := p.Plan(&descriptor.Descriptor{Steps: tt.steps})
			if tt.wantErr {
				if err == nil {
					t.Fatal("atteso errore")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := names(plan); !slices.Equal(got, tt.want) {
				t.Errorf("piano = %v, atteso %v", got, tt.want)
			}
		})
	}
}

func TestPlanStepPersonalizzati(t *testing.T) {
	p := newPlanner(t)
	spec := customspec.Spec{
		Type:   customspec.TypeHTTP,
		Params: map[string]customspec.Param{"zone": {Pattern: "[a-z]+"}},
		Run:    &customspec.Action{URL: "https://cdn.example.com/purge/{{ .Params.zone }}"},
	}
	spec.ApplyDefaults()
	p.Steps = append(p.Steps, config.Step{Name: "cdn-purge", Mode: config.ModeOptional, Enabled: true, MaxRetries: 1, Spec: spec})
	p.Keys = customspec.Keys{[]byte(strings.Repeat("k", 32))}

	plan, err := p.Plan(&descriptor.Descriptor{StepParams: map[string]map[string]string{"cdn-purge": {"zone": "eu"}}})
	if err != nil {
		t.Fatal(err)
	}
	last := plan[len(plan)-1]
	if last.Name != "cdn-purge" || last.Type != "http" || !last.IsCustom() || last.MaxRetries != 1 {
		t.Fatalf("step personalizzato: %+v", last)
	}
	if err := p.Keys.Verify(last.Spec, last.SpecMAC); err != nil || last.SpecHash != customspec.Hash(last.Spec) {
		t.Errorf("firma o hash: %v", err)
	}
	doc, err := customspec.Decode(last.Spec)
	if err != nil || doc.Name != "cdn-purge" || doc.Run.URL != spec.Run.URL {
		t.Errorf("specifica fotografata: %+v, %v", doc, err)
	}
	pub := plan.Public()
	if pub[len(pub)-1].Spec != "" || pub[len(pub)-1].SpecMAC != "" || pub[len(pub)-1].SpecHash == "" || plan[len(plan)-1].Spec == "" {
		t.Errorf("piano pubblico: %+v", pub[len(pub)-1])
	}

	for name, sp := range map[string]map[string]map[string]string{
		"step standard":         {"deploy": {"a": "1"}},
		"step inesistente":      {"nope": {"a": "1"}},
		"parametro sconosciuto": {"cdn-purge": {"x": "1"}},
		"valore non conforme":   {"cdn-purge": {"zone": "EU; rm"}},
	} {
		if _, err := p.Plan(&descriptor.Descriptor{StepParams: sp}); err == nil {
			t.Errorf("%s: atteso errore", name)
		}
	}
	if plan, _ := p.Plan(&descriptor.Descriptor{Steps: map[string]bool{"cdn-purge": false}}); slices.Contains(names(plan), "cdn-purge") {
		t.Error("lo step opzionale disattivato è nel piano")
	}
}
