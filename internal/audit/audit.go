// Package audit registra chi ha fatto cosa: richieste di deploy, annullamenti, modifiche alle
// schedule, accessi negati ed esiti finali dei deploy. Le voci vanno al log applicativo (e
// quindi al SIEM) e, se configurato, a un file JSON Lines in sola aggiunta.
package audit

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Esiti di una voce di audit.
const (
	OutcomeAllowed   = "allowed"   // azione autorizzata ed eseguita
	OutcomeDenied    = "denied"    // azione negata dai permessi
	OutcomeError     = "error"     // azione autorizzata ma fallita
	OutcomeSucceeded = "succeeded" // deploy concluso con successo
	OutcomeFailed    = "failed"    // deploy concluso con errore
)

// Entry è una voce di audit. Non deve contenere segreti.
type Entry struct {
	Time        time.Time `json:"time"`
	Actor       string    `json:"actor"`
	Action      string    `json:"action"`
	Outcome     string    `json:"outcome"`
	Application string    `json:"application,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Deployment  string    `json:"deployment,omitempty"`
	Schedule    string    `json:"schedule,omitempty"`
	DryRun      bool      `json:"dry_run,omitempty"`
	Rollback    string    `json:"rollback,omitempty"`
	// Comment è il commento di un'approvazione o il motivo di una deroga al freeze.
	Comment string `json:"comment,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// PrevHash è l'hash della voce precedente nel file di audit (catena).
	PrevHash string `json:"prev_hash,omitempty"`
}

// Recorder registra le voci di audit. Un errore di registrazione non deve bloccare l'azione,
// ma va segnalato.
type Recorder interface {
	Record(ctx context.Context, e Entry) error
}

// LogRecorder scrive le voci sul logger, che le inoltra anche al SIEM.
type LogRecorder struct {
	Logger *slog.Logger
}

// Record implementa Recorder.
func (r LogRecorder) Record(ctx context.Context, e Entry) error {
	level := slog.LevelInfo
	if e.Outcome == OutcomeDenied {
		level = slog.LevelWarn
	}
	attrs := []any{"audit", true, "actor", e.Actor, "action", e.Action, "outcome", e.Outcome}
	for _, kv := range [][2]string{
		{"application", e.Application}, {"environment", e.Environment}, {"deployment", e.Deployment},
		{"schedule", e.Schedule}, {"rollback", e.Rollback}, {"comment", e.Comment}, {"reason", e.Reason},
	} {
		if kv[1] != "" {
			attrs = append(attrs, kv[0], kv[1])
		}
	}
	if e.DryRun {
		attrs = append(attrs, "dry_run", true)
	}
	r.Logger.Log(ctx, level, "audit", attrs...)
	return nil
}

// Multi inoltra ogni voce a tutti i recorder.
type Multi []Recorder

// Record implementa Recorder.
func (m Multi) Record(ctx context.Context, e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	var errs []error
	for _, r := range m {
		errs = append(errs, r.Record(ctx, e))
	}
	return errors.Join(errs...)
}
