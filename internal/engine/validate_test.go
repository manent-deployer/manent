package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/envconfig"
	"github.com/joeg-ita/manent/internal/governance"
)

func TestValidate(t *testing.T) {
	_, planner := testRegistry(t, artifactStep{})
	gov, err := governance.New(
		[]governance.Rule{{Name: "prod", Applications: []string{"*"}, Environments: []string{"prod"}, Requires: []string{"test"}, Approvals: governance.Approvals{Required: 2}}},
		[]governance.Window{{Name: "sempre", Applications: []string{"*"}, Environments: []string{"prod"}, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Validate does not access the database: an Engine without a client is enough.
	e := NewService(ServiceOptions{Planner: planner, Logger: discard, Resolver: newFakeResolver("r7"), Governance: gov})
	ctx := context.Background()

	d := appDescriptor("prod", "1.0.0")
	d.Secrets = []descriptor.SecretRef{{Name: "chiave", Ref: "vault:segreto/chiave"}}
	v, err := e.Validate(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid || len(v.Plan) != 1 || v.Config["revision"] != "r7" || v.Deploy.Params["version"] != "r7" {
		t.Errorf("deploy effettivo inatteso: %+v", v)
	}
	if len(v.Secrets) != 1 || v.Secrets[0] != "chiave" {
		t.Errorf("segreti: attesi solo i nomi, ricevuto %v", v.Secrets)
	}
	if g := v.Governance; g == nil || g.Rule != "prod" || g.Approvals != 2 || g.Freeze == nil || g.Freeze.Name != "sempre" {
		t.Errorf("governance inattesa: %+v", v.Governance)
	}

	// Policy violated: invalid request. Configuration unreachable: only a warning.
	e.resolver = policyResolver{}
	d.Application = "vietata"
	if _, err := e.Validate(ctx, d); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso ErrInvalidRequest, ricevuto %v", err)
	}
	d.Application = "irraggiungibile"
	if v, err := e.Validate(ctx, d); err != nil || len(v.Warnings) != 1 {
		t.Errorf("configurazione irraggiungibile: %+v %v", v, err)
	}
}

func TestValidateRollout(t *testing.T) {
	_, planner := testRegistry(t, waveStep{name: "deploy", log: &waveLog{}})
	e := NewService(ServiceOptions{Planner: planner, Logger: discard, Resolver: newFakeResolver("r1"), Health: &fakeChecker{}})
	d := waveDescriptor("v-ondate")
	v, err := e.Validate(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if v.Rollout == nil || v.Rollout.Status != "planned" || len(v.Rollout.Stages) != 4 || v.Rollout.Stages[3].Name != "resto/eu-2b+us-1b" {
		t.Fatalf("piano a ondate: %+v", v.Rollout)
	}
	// A target without a wave makes the request invalid.
	d.Deploy.Targets = append(d.Deploy.Targets, descriptor.Target{Host: "ap-1a", Labels: map[string]string{"region": "ap-1", "az": "ap-1a"}})
	if _, err := e.Validate(context.Background(), d); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "ap-1a belongs to no wave") {
		t.Errorf("target scoperto: %v", err)
	}
	// An onFailure with rollback without rollback enabled is rejected.
	d = waveDescriptor("v-ondate-2")
	d.Rollout.OnFailure = descriptor.OnFailureRollbackAll
	if _, err := e.Validate(context.Background(), d); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "rollback enabled") {
		t.Errorf("rollback non attivo: %v", err)
	}
}

func TestValidateEnvironment(t *testing.T) {
	_, planner := testRegistry(t, waveStep{name: "deploy", log: &waveLog{}})
	e := NewService(ServiceOptions{Planner: planner, Logger: discard, Resolver: envconfig.Resolver{}, Health: &fakeChecker{}})
	ctx := context.Background()
	d := appDescriptor("prod", "1.0.0")
	check := func(config string) (*Validation, error) {
		return e.ValidateEnvironment(ctx, EnvironmentCheck{Application: d.Application, Environment: d.Environment, Config: config, Descriptor: d})
	}
	v, err := check("plugin: remote-copy\ntargets:\n  - {host: web-1, labels: {region: eu-1, az: eu-1a}}\n  - {host: web-2, labels: {region: eu-1, az: eu-1b}}\nrollout:\n  waves: [{name: tutto, select: \"region=eu-1\", by: az}]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid || v.Config["source"] != "candidate configuration" || len(v.Deploy.Targets) != 2 || v.Rollout == nil || len(v.Rollout.Stages) != 2 {
		t.Errorf("validazione: %+v", v)
	}
	for want, config := range map[string]string{
		"field plugn not found": "plugn: remote-copy\n",
		"belongs to no wave":    "targets:\n  - {host: web-1, labels: {region: eu-1, az: eu-1a}}\n  - {host: us-1, labels: {region: us-1, az: us-1a}}\nrollout:\n  waves: [{name: eu, select: \"region=eu-1\", by: az}]\n",
		"empty configuration":   " ",
	} {
		if _, err := check(config); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso %q, ricevuto %v", want, err)
		}
	}
	// The example descriptor must be of the environment checked.
	if _, err := e.ValidateEnvironment(ctx, EnvironmentCheck{Application: "altra", Environment: "prod", Config: "plugin: x\n", Descriptor: d}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("descrittore di un altro ambiente: %v", err)
	}
}
