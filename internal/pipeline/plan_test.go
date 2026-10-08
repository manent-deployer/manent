package pipeline

import (
	"context"
	"slices"
	"testing"

	"github.com/joeg-ita/go-dart/internal/config"
	"github.com/joeg-ita/go-dart/internal/descriptor"
)

// stubStep è uno step vuoto usato nei test.
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
