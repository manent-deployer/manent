// Package deploylog collects the logs of every deploy in a durable DBOS stream of its
// workflow, readable from API and CLI even when the deploy runs on another worker.
//
// The worker marks the context of every step with WithStep; Handler, part of the slog chain,
// writes to the stream the records emitted with that context (InfoContext, WarnContext, ...).
// Logs still go to the application log and the SIEM too. Like those, they must not contain
// secrets: the stream is persisted in the system database.
package deploylog

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/service"
)

// StreamKey is the key of the DBOS stream with the deploy logs.
const StreamKey = "log"

// Per-step limits, because the stream lives in the system database.
const (
	maxLinesPerStep = 500
	maxMessage      = 2000
	maxAttrValue    = 500
)

// Line is a line of a deploy log (contract in internal/service).
type Line = service.LogLine

// stepKey is the context key of the running step.
type stepKey struct{}

// stepInfo identifies the running step. ctx is the DBOS context of the step: it stays
// reachable even if the step code creates derived contexts (timeouts, tracing).
type stepInfo struct {
	ctx   dbos.Context
	step  string
	lines atomic.Int32
}

// WithStep marks the context of a DBOS step: logs emitted with the returned context (or a
// derived one) go to the deploy stream. ctx must be the context received by the step
// function; otherwise it is returned unchanged.
func WithStep(ctx context.Context, step string) context.Context {
	dc, ok := ctx.(dbos.Context)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, stepKey{}, &stepInfo{ctx: dc, step: step})
}

// Handler wraps a slog handler and copies to the deploy stream the records emitted in the
// context of a step.
type Handler struct {
	next  slog.Handler
	attrs []slog.Attr
	group string
}

// NewHandler creates the handler on top of next.
func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }

// Enabled implements slog.Handler. The deploy log collects records from INFO up even if the
// application log has a higher level (e.g. warn).
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	if _, ok := ctx.Value(stepKey{}).(*stepInfo); ok && l >= slog.LevelInfo {
		return true
	}
	return h.next.Enabled(ctx, l)
}

// Handle implements slog.Handler. A stream write error does not block logging.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if si, ok := ctx.Value(stepKey{}).(*stepInfo); ok && r.Level >= slog.LevelInfo {
		h.write(si, r)
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *Handler) write(si *stepInfo, r slog.Record) {
	n := si.lines.Add(1)
	if n > maxLinesPerStep {
		return
	}
	line := Line{Time: r.Time.UTC(), Level: r.Level.String(), Step: si.step, Message: truncate(r.Message, maxMessage)}
	if n == maxLinesPerStep {
		line.Message, line.Level = fmt.Sprintf("limit of %d lines per step reached: the next ones are only in the application log", maxLinesPerStep), slog.LevelWarn.String()
	}
	attrs := map[string]string{}
	add := func(prefix string, a slog.Attr) {
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		attrs[key] = truncate(a.Value.Resolve().String(), maxAttrValue)
	}
	for _, a := range h.attrs {
		add("", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(h.group, a)
		return true
	})
	if len(attrs) > 0 {
		line.Attrs = attrs
	}
	_ = dbos.WriteStream(si.ctx, StreamKey, line)
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &c
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	if c.group != "" {
		name = c.group + "." + name
	}
	c.group = name
	return &c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Page is a portion of a deploy log.
type Page = service.LogPage

// Read reads the log lines from the given offset, without waiting for new ones. Done is set
// only if the stream was closed: the workflow state must be checked separately.
func Read(c dbos.Client, workflowID string, offset int) (Page, error) {
	lines, done, err := dbos.ReadStream[Line](c, workflowID, StreamKey, dbos.WithReadStreamSnapshot(), dbos.WithReadStreamFromOffset(offset))
	if err != nil {
		return Page{}, err
	}
	if lines == nil {
		lines = []Line{}
	}
	return Page{Lines: lines, Next: offset + len(lines), Done: done}, nil
}

// Copy appends to the current workflow's stream the log lines of another workflow (e.g. the
// child that ran a rollout pass on a region worker). ctx must be the context of a DBOS step.
// It returns the copied lines.
func Copy(ctx context.Context, src dbos.Client, workflowID string) (int, error) {
	dc, ok := ctx.(dbos.Context)
	if !ok {
		return 0, fmt.Errorf("deploylog.Copy: requires the context of a DBOS step")
	}
	lines, _, err := dbos.ReadStream[Line](src, workflowID, StreamKey, dbos.WithReadStreamSnapshot())
	if err != nil {
		return 0, err
	}
	for _, l := range lines {
		if err := dbos.WriteStream(dc, StreamKey, l); err != nil {
			return 0, err
		}
	}
	return len(lines), nil
}
