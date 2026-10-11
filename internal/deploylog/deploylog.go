// Package deploylog raccoglie i log di ogni deploy in uno stream durevole DBOS del suo
// workflow, consultabile da API e CLI anche quando il deploy è eseguito da un altro worker.
//
// Il worker marca il context di ogni step con WithStep; Handler, inserito nella catena di
// slog, scrive nello stream i record emessi con quel context (InfoContext, WarnContext, ...).
// I log continuano ad andare anche al log applicativo e al SIEM. Come quelli, non devono
// contenere segreti: lo stream è persistito nel database di sistema.
package deploylog

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/service"
)

// StreamKey è la chiave dello stream DBOS con i log del deploy.
const StreamKey = "log"

// Limiti per step, perché lo stream sta nel database di sistema.
const (
	maxLinesPerStep = 500
	maxMessage      = 2000
	maxAttrValue    = 500
)

// Line è una riga del log di un deploy (contratto in internal/service).
type Line = service.LogLine

// stepKey è la chiave del context con lo step in esecuzione.
type stepKey struct{}

// stepInfo identifica lo step in esecuzione. ctx è il context DBOS dello step: resta
// raggiungibile anche se il codice dello step crea context derivati (timeout, tracing).
type stepInfo struct {
	ctx   dbos.Context
	step  string
	lines atomic.Int32
}

// WithStep marca il context di uno step DBOS: i log emessi con il context restituito (o con
// uno derivato) finiscono nello stream del deploy. ctx deve essere il context ricevuto dalla
// funzione dello step; altrimenti viene restituito invariato.
func WithStep(ctx context.Context, step string) context.Context {
	dc, ok := ctx.(dbos.Context)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, stepKey{}, &stepInfo{ctx: dc, step: step})
}

// Handler avvolge un handler slog e copia nello stream del deploy i record emessi nel
// context di uno step.
type Handler struct {
	next  slog.Handler
	attrs []slog.Attr
	group string
}

// NewHandler crea l'handler sopra next.
func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }

// Enabled implementa slog.Handler. Il log del deploy raccoglie i record da INFO in su anche
// se il log applicativo ha un livello più alto (es. warn).
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	if _, ok := ctx.Value(stepKey{}).(*stepInfo); ok && l >= slog.LevelInfo {
		return true
	}
	return h.next.Enabled(ctx, l)
}

// Handle implementa slog.Handler. Un errore di scrittura nello stream non blocca il log.
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
		line.Message, line.Level = fmt.Sprintf("limite di %d righe per step raggiunto: le successive sono solo nel log applicativo", maxLinesPerStep), slog.LevelWarn.String()
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

// WithAttrs implementa slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &c
}

// WithGroup implementa slog.Handler.
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

// Page è una porzione del log di un deploy.
type Page = service.LogPage

// Read legge le righe del log dall'offset indicato, senza attendere quelle nuove. Done è
// valorizzato solo se lo stream è stato chiuso: lo stato del workflow va verificato a parte.
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

// Copy aggiunge allo stream del workflow corrente le righe di log di un altro workflow (es. il
// figlio che ha eseguito un passo del rollout su un worker di region). ctx deve essere il
// context di uno step DBOS. Restituisce le righe copiate.
func Copy(ctx context.Context, src dbos.Client, workflowID string) (int, error) {
	dc, ok := ctx.(dbos.Context)
	if !ok {
		return 0, fmt.Errorf("deploylog.Copy: serve il context di uno step DBOS")
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
