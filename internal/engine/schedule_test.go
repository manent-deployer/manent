package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeg-ita/manent/internal/descriptor"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// countStep counts runs safely across goroutines.
type countStep struct{ n *atomic.Int32 }

func (countStep) Name() string { return "count" }

func (s countStep) Run(context.Context, pipeline.Input) (pipeline.Output, error) {
	s.n.Add(1)
	return pipeline.Output{}, nil
}

func scheduled(s descriptor.Schedule, id string) *descriptor.Descriptor {
	d := testDescriptor(id)
	d.Schedule = &s
	return d
}

func TestDeployPianificatoAt(t *testing.T) {
	var n atomic.Int32
	e := newEngine(t, countStep{&n})
	ctx := context.Background()

	at := time.Now().Add(1500 * time.Millisecond)
	sub, err := e.Submit(ctx, scheduled(descriptor.Schedule{At: &at}, "at-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.State != "DELAYED" || sub.ScheduledAt == nil {
		t.Fatalf("submission inattesa: %+v", sub)
	}
	st, err := e.Status(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "DELAYED" || st.ScheduledAt == nil || n.Load() != 0 {
		t.Fatalf("il deploy non doveva ancora partire: %+v", st)
	}
	if _, err := e.Wait(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(at) || n.Load() != 1 {
		t.Errorf("deploy eseguito prima dell'orario o più volte (esecuzioni: %d)", n.Load())
	}

	// Resubmitting the same descriptor after its time (e.g. a retrying CI): it returns the
	// existing deploy instead of rejecting the time as past.
	again, err := e.Submit(ctx, scheduled(descriptor.Schedule{At: &at}, "at-1"))
	if err != nil {
		t.Fatalf("reinvio dopo l'orario: %v", err)
	}
	if again.ID != sub.ID || again.State != "SUCCESS" || n.Load() != 1 {
		t.Errorf("reinvio non idempotente: %+v (esecuzioni: %d)", again, n.Load())
	}
	// Another id with a past time is still an error.
	if _, err := e.Submit(ctx, scheduled(descriptor.Schedule{At: &at}, "at-2")); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("atteso ErrInvalidRequest, ricevuto %v", err)
	}
}

func TestDeployPianificatoNelPassato(t *testing.T) {
	var n atomic.Int32
	e := newEngine(t, countStep{&n})
	at := time.Now().Add(-time.Minute)
	if _, err := e.Submit(context.Background(), scheduled(descriptor.Schedule{At: &at}, "")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("atteso ErrInvalidRequest, ricevuto %v", err)
	}
}

func TestCancelDeployPianificato(t *testing.T) {
	var n atomic.Int32
	e := newEngine(t, countStep{&n})
	ctx := context.Background()

	at := time.Now().Add(time.Hour)
	sub, err := e.Submit(ctx, scheduled(descriptor.Schedule{At: &at}, "cancel-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "CANCELLED" {
		t.Errorf("stato atteso CANCELLED, ricevuto %s", st.State)
	}
	if err := e.Cancel(ctx, "inesistente"); !errors.Is(err, ErrNotFound) {
		t.Errorf("atteso ErrNotFound, ricevuto %v", err)
	}
}

func TestScheduleRicorrente(t *testing.T) {
	var n atomic.Int32
	e := newEngine(t, countStep{&n})
	ctx := context.Background()

	sub, err := e.Submit(ctx, scheduled(descriptor.Schedule{Cron: "* * * * * *", Name: "ogni-secondo"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if sub.State != StateScheduled || sub.Schedule != "ogni-secondo" {
		t.Fatalf("submission inattesa: %+v", sub)
	}

	// Applying the same descriptor again updates the schedule instead of duplicating it.
	if _, err := e.Submit(ctx, scheduled(descriptor.Schedule{Cron: "* * * * * *", Name: "ogni-secondo"}, "")); err != nil {
		t.Fatal(err)
	}
	list, err := e.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Application != "app" || list[0].Status != "ACTIVE" {
		t.Fatalf("schedule inattese: %+v", list)
	}

	deadline := time.Now().Add(10 * time.Second)
	for n.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n.Load() < 2 {
		t.Fatalf("attese almeno 2 esecuzioni, ricevute %d", n.Load())
	}
	deps, err := e.List(ctx, ListFilter{Schedule: "ogni-secondo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) < 2 || !strings.HasPrefix(deps[0].ID, "ogni-secondo@") {
		t.Fatalf("deploy della schedule inattesi: %+v", deps)
	}

	if err := e.PauseSchedule(ctx, "ogni-secondo"); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.GetSchedule(ctx, "ogni-secondo"); s.Status != "PAUSED" {
		t.Errorf("stato atteso PAUSED, ricevuto %s", s.Status)
	}
	if err := e.DeleteSchedule(ctx, "ogni-secondo"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.GetSchedule(ctx, "ogni-secondo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("atteso ErrNotFound dopo l'eliminazione, ricevuto %v", err)
	}
}
