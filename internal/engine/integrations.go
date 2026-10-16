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

// ChatNotifier sends chat notifications to a destination by name (internal/chat).
type ChatNotifier interface {
	// Wants reports whether the destination receives the event.
	Wants(route, event string) bool
	// Send sends the message or, with prev, updates the one already sent.
	Send(ctx context.Context, route string, msg chat.Message, prev *chat.Ref) (*chat.Ref, error)
}

// notifyChat sends the message to every destination of the deploy, each in a DBOS step
// (notify:<destination>:<event>). The step always exists for every destination recorded in
// the request: if the destination does not want the event, or is no longer configured, it
// sends nothing. So the step sequence does not depend on the configuration read during a
// recovery. prev are the messages to update; it returns the references of the sent messages.
// A failed send is logged and does not stop the workflow.
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
		}, dbos.WithStepName("notify:"+route+":"+msg.Event), dbos.WithStepMaxRetries(w.callbackRetries),
			dbos.WithStepRetryPredicate(pipeline.Retryable))
		if err != nil {
			w.logger.Warn("notifica in chat non inviata", "deployment", id, "route", route, "event", msg.Event, "error", err)
			ref = before
		}
		refs[route] = ref
	}
	return refs
}

// chatMessage prepares the message of a deploy.
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

// changeRequest describes the deploy to the change management system.
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

// changeErr makes permanent the ITSM errors a retry cannot fix.
func changeErr(err error) error {
	if errors.Is(err, change.ErrPermanent) {
		return pipeline.Permanent(err)
	}
	return err
}

// errNoChange is the error of a deploy requiring a change without a configured system.
var errNoChange = pipeline.Permanent(errors.New("il deploy richiede un ticket di change ma integrations.change non è configurato su questo worker"))

// openChange opens (or finds) the deploy's change ticket and moves it to implementation, in
// the change:open DBOS step. The deploy does not start without a ticket. The ticket is
// immediately visible in the deploy state.
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
			// The ticket exists: a rejected state change does not block the deploy.
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

// closeChange records the deploy outcome in the ticket and closes it, in the change:close
// step. An error is logged: the deploy has already ended.
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

// ticketFor rebuilds the ITSM ticket from the deploy's ticket.
func (w *Worker) ticketFor(t *ChangeTicket) change.Ticket {
	return change.Ticket{ID: t.ID, Number: t.Number, URL: t.URL, Approval: t.Approval}
}
