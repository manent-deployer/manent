package rollout

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
)

// topology creates 3 regions × 3 AZs with 2 nodes per AZ; the first eu-1a node is the canary.
func topology() []descriptor.Target {
	var ts []descriptor.Target
	for _, reg := range []string{"us-1", "eu-1", "eu-2"} {
		for _, az := range []string{"a", "b", "c"} {
			for n := 1; n <= 2; n++ {
				l := map[string]string{"region": reg, "az": reg + az}
				if reg == "eu-1" && az == "a" && n == 1 {
					l["canary"] = "true"
				}
				ts = append(ts, descriptor.Target{Host: fmt.Sprintf("%s%s-%d", reg, az, n), Labels: l})
			}
		}
	}
	return ts
}

func designRollout() *descriptor.Rollout {
	return &descriptor.Rollout{
		Regions: []string{"eu-1", "eu-2", "us-1"},
		Waves: []descriptor.Wave{
			{Name: "canary", Select: "canary=true", Bake: "15m"},
			{Name: "eu-1", Select: "region=eu-1, canary!=true", By: "az", Bake: "10m"},
			{Name: "resto", Select: "region in (eu-2, us-1)", By: "az", ParallelRegions: 2, Bake: "10m"},
		},
		MaxUnavailable: "50%",
	}
}

func TestPianoDelDesign(t *testing.T) {
	p, err := Build(topology(), designRollout())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Stages {
		names = append(names, s.Name)
	}
	want := "canary eu-1/eu-1a eu-1/eu-1b eu-1/eu-1c resto/eu-2a+us-1a resto/eu-2b+us-1b resto/eu-2c+us-1c"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("stadi:\n %s\natteso\n %s", got, want)
	}
	if p.TotalPasses() != 1+3+6 {
		t.Errorf("passi: %d", p.TotalPasses())
	}
	c := p.Stages[0]
	if c.Bake != 15*time.Minute || len(c.Passes[0].Targets) != 1 || c.Passes[0].Region != "eu-1" || c.Passes[0].AZ != "eu-1a" || !c.LastOfWave {
		t.Errorf("canary inattesa: %+v", c)
	}
	// eu-1a without the canary: 1 node; eu-1b: 2 nodes in batches of 1 (50%).
	if n := len(p.Stages[1].Passes[0].Targets); n != 1 {
		t.Errorf("eu-1a: %d target", n)
	}
	if b := p.Stages[2].Passes[0].Batches; len(b) != 2 || len(b[0]) != 1 {
		t.Errorf("lotti di eu-1b: %+v", b)
	}
	last := p.Stages[6]
	if len(last.Passes) != 2 || last.Passes[0].Name != "resto/eu-2/eu-2c" || last.Passes[1].Region != "us-1" || !last.LastOfWave || p.Stages[5].LastOfWave {
		t.Errorf("ultimo stadio inatteso: %+v", last)
	}
}

func TestPianoNonValido(t *testing.T) {
	ts := topology()
	r := designRollout()
	r.Waves = r.Waves[:2] // eu-2 and us-1 without a wave
	if _, err := Build(ts, r); err == nil || !strings.Contains(err.Error(), "non appartiene ad alcuna ondata") {
		t.Errorf("target scoperti: %v", err)
	}
	r = designRollout()
	r.Waves[1].Select = "region=eu-1" // the canary is in two waves
	if _, err := Build(ts, r); err == nil || !strings.Contains(err.Error(), "sia da") {
		t.Errorf("sovrapposizione: %v", err)
	}
	r = designRollout()
	r.Waves = append(r.Waves, descriptor.Wave{Name: "vuota", Select: "region=ap-1"})
	if _, err := Build(ts, r); err == nil || !strings.Contains(err.Error(), "non seleziona") {
		t.Errorf("ondata vuota: %v", err)
	}
	ts[3].Labels = map[string]string{"region": "us-1"} // without az
	if _, err := Build(ts, designRollout()); err == nil || !strings.Contains(err.Error(), "etichette") {
		t.Errorf("etichette mancanti: %v", err)
	}
	r = designRollout()
	r.OnFailure = "riprova"
	r.MaxUnavailable = "0"
	if _, err := Build(topology(), r); err == nil || !strings.Contains(err.Error(), "onFailure") || !strings.Contains(err.Error(), "maxUnavailable") {
		t.Errorf("validazione: %v", err)
	}
}

func TestPianoSenzaDivisione(t *testing.T) {
	r := &descriptor.Rollout{Waves: []descriptor.Wave{{Name: "tutto", Select: "region in (eu-1, eu-2, us-1)"}}, MaxUnavailable: "6"}
	p, err := Build(topology(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) != 1 || len(p.Stages[0].Passes[0].Batches) != 3 || p.Stages[0].Passes[0].Region != "" {
		t.Errorf("piano inatteso: %+v", p.Stages)
	}
}

func TestPianoGatePerLotto(t *testing.T) {
	r := designRollout()
	r.Waves[1].BatchBake = "1m"
	r.Waves[2].BatchBake = "0s"
	p, err := Build(topology(), r)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range p.Stages {
		for _, ps := range st.Passes {
			want, bake := ps.Wave != "canary", time.Duration(0)
			if ps.Wave == "eu-1" {
				bake = time.Minute
			}
			if ps.BatchGate != want || ps.BatchBake != bake {
				t.Errorf("passo %s: gate %v bake %s", ps.Name, ps.BatchGate, ps.BatchBake)
			}
		}
	}
}
