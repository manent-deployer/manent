package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/manent-deployer/manent/internal/descriptor"
)

func TestIDDiUnAltroDeployRifiutato(t *testing.T) {
	var runs []string
	e := newEngine(t, recordStep{name: "a", runs: &runs})
	ctx := context.Background()
	sub, err := e.Submit(ctx, testDescriptor("shared-id"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	// The same request again is idempotent.
	if _, err := e.Submit(ctx, testDescriptor("shared-id")); err != nil {
		t.Fatalf("reinvio dello stesso deploy: %v", err)
	}
	// Another application or environment must not receive the existing deploy.
	for _, f := range []func(d *descriptor.Descriptor){
		func(d *descriptor.Descriptor) { d.Application = "other" },
		func(d *descriptor.Descriptor) { d.Environment = "prod" },
	} {
		d := testDescriptor("shared-id")
		f(d)
		if _, err := e.Submit(ctx, d); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s/%s con un ID già usato: %v", d.Application, d.Environment, err)
		}
	}
	if len(runs) != 1 {
		t.Errorf("esecuzioni: %v", runs)
	}
}
