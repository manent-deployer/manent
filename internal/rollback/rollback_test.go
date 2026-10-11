package rollback

import (
	"slices"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/config"
	"github.com/joeg-ita/manent/internal/descriptor"
)

func testPolicy() Policy {
	return NewPolicy(config.Default(), []string{"plugin", "cmdb"})
}

func descriptorOf(artifactType string, rb *descriptor.Rollback) descriptor.Descriptor {
	return descriptor.Descriptor{Artifact: descriptor.Artifact{Type: artifactType}, Rollback: rb}
}

func ptr(b bool) *bool { return &b }

func TestApply(t *testing.T) {
	p := testPolicy()
	p.Types["application"] = config.RollbackType{Allowed: true, Enabled: true}

	// Default del tipo: attivo, con gli step e il locator di default.
	d, err := p.Apply(descriptorOf("application", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Rollback.IsEnabled() || !slices.Equal(d.Rollback.On, config.DefaultRollbackOn) || d.Rollback.Locator != "plugin" {
		t.Errorf("decisione inattesa: %+v", d.Rollback)
	}

	// Il descrittore può disattivarlo o restringere gli step e scegliere il locator.
	d, err = p.Apply(descriptorOf("application", &descriptor.Rollback{Enabled: ptr(false)}))
	if err != nil || d.Rollback.IsEnabled() {
		t.Errorf("atteso rollback disattivato: %+v %v", d.Rollback, err)
	}
	d, err = p.Apply(descriptorOf("application", &descriptor.Rollback{On: []string{"post-test"}, Locator: "cmdb"}))
	if err != nil || !d.Rollback.Triggers("post-test") || d.Rollback.Triggers("deploy") || d.Rollback.Locator != "cmdb" {
		t.Errorf("decisione inattesa: %+v %v", d.Rollback, err)
	}
}

func TestApplyNonAmmesso(t *testing.T) {
	p := testPolicy()
	// migration è esclusa di default: chiedere il rollback è un errore, non chiederlo no.
	if _, err := p.Apply(descriptorOf("migration", &descriptor.Rollback{Enabled: ptr(true)})); err == nil || !strings.Contains(err.Error(), "non ammesso") {
		t.Errorf("atteso rollback non ammesso, ricevuto %v", err)
	}
	if d, err := p.Apply(descriptorOf("migration", nil)); err != nil || d.Rollback.IsEnabled() {
		t.Errorf("atteso rollback disattivato senza errore: %+v %v", d.Rollback, err)
	}
	// Tipo non elencato: non ammesso.
	if _, err := p.Apply(descriptorOf("container", &descriptor.Rollback{Enabled: ptr(true)})); err == nil {
		t.Error("atteso errore per un tipo non elencato")
	}
}

func TestApplyErrori(t *testing.T) {
	p := testPolicy()
	if _, err := p.Apply(descriptorOf("application", &descriptor.Rollback{Enabled: ptr(true), On: []string{"inesistente"}})); err == nil {
		t.Error("atteso errore per uno step sconosciuto")
	}
	if _, err := p.Apply(descriptorOf("application", &descriptor.Rollback{Enabled: ptr(true), Locator: "nessuno"})); err == nil {
		t.Error("atteso errore per un locator sconosciuto")
	}
}
