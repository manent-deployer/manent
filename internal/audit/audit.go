// Package audit records who did what: deploy requests, cancellations, schedule changes,
// denied accesses and final deploy outcomes. Entries go to the application log (and so to
// the SIEM) and, if configured, to an append-only JSON Lines file.
package audit

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Outcomes of an audit entry.
const (
	OutcomeAllowed   = "allowed"   // action authorized and done
	OutcomeDenied    = "denied"    // action denied by permissions
	OutcomeError     = "error"     // action authorized but failed
	OutcomeSucceeded = "succeeded" // deploy ended successfully
	OutcomeFailed    = "failed"    // deploy ended with an error
)

// Entry is an audit entry. It must not contain secrets.
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
	// Step, Spec and Targets describe the run of a custom step: name, sha256 of the spec and
	// targets reached.
	Step    string   `json:"step,omitempty"`
	Spec    string   `json:"spec,omitempty"`
	Targets []string `json:"targets,omitempty"`
	// Comment is the comment of an approval or the reason of a freeze override.
	Comment string `json:"comment,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// AuthzRevision is the permission policy revision that decided (rbac.engine: opa); Grant the
	// grant of that policy that allowed the action.
	AuthzRevision string `json:"authz_revision,omitempty"`
	Grant         string `json:"grant,omitempty"`
	// PrevHash is the hash of the previous entry in the audit file (chain).
	PrevHash string `json:"prev_hash,omitempty"`
}

// Recorder records audit entries. A recording error must not block the action, but must be
// reported.
type Recorder interface {
	Record(ctx context.Context, e Entry) error
}

// LogRecorder writes entries to the logger, which also forwards them to the SIEM.
type LogRecorder struct {
	Logger *slog.Logger
}

// Record implements Recorder.
func (r LogRecorder) Record(ctx context.Context, e Entry) error {
	level := slog.LevelInfo
	if e.Outcome == OutcomeDenied {
		level = slog.LevelWarn
	}
	attrs := []any{"audit", true, "actor", e.Actor, "action", e.Action, "outcome", e.Outcome}
	for _, kv := range [][2]string{
		{"application", e.Application}, {"environment", e.Environment}, {"deployment", e.Deployment},
		{"schedule", e.Schedule}, {"rollback", e.Rollback}, {"step", e.Step}, {"spec", e.Spec},
		{"comment", e.Comment}, {"reason", e.Reason}, {"authz_revision", e.AuthzRevision}, {"grant", e.Grant},
	} {
		if kv[1] != "" {
			attrs = append(attrs, kv[0], kv[1])
		}
	}
	if len(e.Targets) > 0 {
		attrs = append(attrs, "targets", e.Targets)
	}
	if e.DryRun {
		attrs = append(attrs, "dry_run", true)
	}
	r.Logger.Log(ctx, level, "audit", attrs...)
	return nil
}

// Multi forwards every entry to all recorders.
type Multi []Recorder

// Record implements Recorder.
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
