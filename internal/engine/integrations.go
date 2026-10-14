package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/joeg-ita/manent/internal/callback"
	"github.com/joeg-ita/manent/internal/change"
	"github.com/joeg-ita/manent/internal/chat"
	"github.com/joeg-ita/manent/internal/pipeline"
)

// ChatNotifier invia le notifiche in chat a una destinazione per nome (internal/chat).
type ChatNotifier interface {
	// Wants indica se la destinazione riceve l'evento.
	Wants(route, event string) bool
	// Send invia il messaggio o, con prev, aggiorna quello già inviato.
	Send(ctx context.Context, route string, msg chat.Message, prev *chat.Ref) (*chat.Ref, error)
}

// notifyChat invia il messaggio a ogni destinazione del deploy, ciascuna in uno step DBOS
// (notify:<destinazione>:<evento>). Lo step c'è sempre, per ogni destinazione registrata
// nella richiesta: se la destinazione non vuole l'evento, o non è più configurata, non invia
// nulla. Così la sequenza degli step non dipende dalla configurazione letta durante una
// recovery. prev sono i messaggi da aggiornare; restituisce i riferimenti dei messaggi inviati.
// Un invio fallito viene registrato e non ferma il workflow.
func (w *Worker) notifyChat(ctx dbos.Context, id string, routes []string, msg chat.Message, prev map[string]*chat.Ref) map[string]*chat.Ref {
	if len(routes) == 0 {
		return nil
	}
	refs := map[string]*chat.Ref{}
	for _, route := range routes {
		var before *chat.Ref
		if prev != nil {
			before = prev[route]
		}
		ref, err := dbos.RunAsStep(ctx, func(c context.Context) (*chat.Ref, error) {
			if w.chat == nil || !w.chat.Wants(route, msg.Event) {
				return before, nil
			}
			return w.chat.Send(c, route, msg, before)
		}, dbos.WithStepName("notify:"+route+":"+msg.Event), dbos.WithStepMaxRetries(w.callbackRetries))
		if err != nil {
			w.logger.Warn("notifica in chat non inviata", "deployment", id, "route", route, "event", msg.Event, "error", err)
			ref = before
		}
		refs[route] = ref
	}
	return refs
}

// chatMessage prepara il messaggio di un deploy.
func (w *Worker) chatMessage(id string, req Request, event string) chat.Message {
	d := req.Descriptor
	return chat.Message{
		Event: event, DeploymentID: id, Application: d.Application, Environment: d.Environment,
		Artifact: d.Artifact.Name, Version: d.Artifact.Version, Kind: cmp.Or(req.Kind, KindDeploy),
		RequestedBy: req.RequestedBy, DryRun: d.DryRun,
	}
}

func chatChange(t *ChangeTicket) *chat.Change {
	if t == nil {
		return nil
	}
	return &chat.Change{Number: t.Number, URL: t.URL}
}

// changeRequest descrive il deploy al sistema di change management.
func (w *Worker) changeRequest(id string, req Request) change.Request {
	d := req.Descriptor
	r := change.Request{
		DeploymentID: id, Application: d.Application, Environment: d.Environment,
		Artifact: d.Artifact.Name, Version: d.Artifact.Version, Kind: cmp.Or(req.Kind, KindDeploy),
		RequestedBy: req.RequestedBy, Approval: req.Change != nil && req.Change.Approval,
	}
	if w.publicURL != "" {
		r.URL = strings.TrimRight(w.publicURL, "/") + "/deployments/" + id
	}
	return r
}

func ticketOf(t change.Ticket) *ChangeTicket {
	return &ChangeTicket{ID: t.ID, Number: t.Number, URL: t.URL, Approval: t.Approval}
}

// changeErr rende permanenti gli errori del sistema ITSM che un nuovo tentativo non risolve.
func changeErr(err error) error {
	if errors.Is(err, change.ErrPermanent) {
		return pipeline.Permanent(err)
	}
	return err
}

// errNoChange è l'errore di un deploy che richiede un change senza sistema configurato.
var errNoChange = pipeline.Permanent(errors.New("il deploy richiede un ticket di change ma integrations.change non è configurato su questo worker"))

// openChange apre (o ritrova) il ticket di change del deploy e lo porta in implementazione,
// nello step DBOS change:open. Il deploy non parte senza ticket. Il ticket è subito visibile
// nello stato del deploy.
func (w *Worker) openChange(ctx dbos.Context, id string, req Request) (*ChangeTicket, error) {
	t, err := dbos.RunAsStep(ctx, func(c context.Context) (change.Ticket, error) {
		if w.change == nil {
			return change.Ticket{}, errNoChange
		}
		t, err := w.change.Open(c, w.changeRequest(id, req))
		if err != nil {
			return t, changeErr(err)
		}
		if err := w.change.Start(c, t); err != nil {
			// Il ticket esiste: un cambio di stato rifiutato non blocca il deploy.
			w.logger.WarnContext(c, "avvio del change non registrato", "deployment", id, "change", t.Number, "error", err)
		}
		w.logger.InfoContext(c, "change aperto", "deployment", id, "change", t.Number, "system", w.change.Name())
		return t, nil
	}, dbos.WithStepName("change:open"), dbos.WithStepMaxRetries(3), dbos.WithStepRetryPredicate(pipeline.Retryable))
	if err != nil {
		return nil, fmt.Errorf("change management: %w", err)
	}
	ct := ticketOf(t)
	attrs := attributes(req)
	attrs[attrChange], attrs[attrChangeURL] = ct.Number, ct.URL
	if err := dbos.SetWorkflowAttributes(ctx, id, attrs); err != nil {
		w.logger.Warn("aggiornamento degli attributi del deploy fallito", "deployment", id, "error", err)
	}
	return ct, nil
}

// closeChange registra l'esito del deploy nel ticket e lo chiude, nello step change:close.
// Un errore viene registrato: il deploy è già concluso.
func (w *Worker) closeChange(ctx dbos.Context, id string, t *ChangeTicket, final callback.Event) {
	_, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		if w.change == nil {
			return "", errNoChange
		}
		o := change.Outcome{Success: final.Status == callback.StatusSucceeded, Message: final.Error, Rollback: final.Rollback}
		return "", changeErr(w.change.Close(c, w.ticketFor(t), o))
	}, dbos.WithStepName("change:close"), dbos.WithStepMaxRetries(3), dbos.WithStepRetryPredicate(pipeline.Retryable))
	if err != nil {
		w.logger.Error("chiusura del change fallita", "deployment", id, "change", t.Number, "error", err)
	}
}

// ticketFor ricostruisce il ticket del sistema ITSM dal ticket del deploy.
func (w *Worker) ticketFor(t *ChangeTicket) change.Ticket {
	return change.Ticket{ID: t.ID, Number: t.Number, URL: t.URL, Approval: t.Approval}
}
