package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// applySchedule crea o aggiorna (upsert per nome) la schedule DBOS.
// I tick usano la coda interna di DBOS: quella dei deploy è partizionata e non preleva
// workflow senza chiave di partizione. Il deploy accodato dal tick usa invece la coda dei deploy.
func (e *Engine) applySchedule(req Request) (*Submission, error) {
	// Le esecuzioni non appartengono alla traccia della richiesta che ha creato la schedule.
	req.TraceParent = ""
	d := req.Descriptor
	name := d.ScheduleName()
	err := dbos.ApplySchedules(e.client, []dbos.ScheduleSpec{{
		ScheduleName: name,
		WorkflowName: scheduledWorkflowName,
		Schedule:     d.Schedule.CronExpr(),
		CronTimezone: d.Schedule.Timezone,
		Context:      scheduleContext{Request: req, QueueName: e.queueName},
	}})
	if err != nil {
		return nil, err
	}
	e.logger.Info("schedule di deploy applicata", "schedule", name, "cron", d.Schedule.CronExpr(), "application", d.Application, "environment", d.Environment, "requested_by", req.RequestedBy)
	return &Submission{State: StateScheduled, Schedule: name}, nil
}

// scheduleContext è il valore salvato nella schedule DBOS e passato a ogni tick.
type scheduleContext struct {
	Request   Request `json:"request"`
	QueueName string  `json:"queue_name"`
}

// scheduledWorkflow è invocato da DBOS a ogni tick della schedule: accoda un deploy
// con ID deterministico "<schedule>@<orario tick>", così un tick non produce mai due deploy
// anche se più worker lo eseguono. Non attende il deploy, per non occupare uno slot della coda.
func (w *Worker) scheduledWorkflow(ctx dbos.Context, in dbos.ScheduledWorkflowInput) (string, error) {
	sc, err := dbos.DecodeScheduleContext[scheduleContext](in)
	if err != nil {
		return "", fmt.Errorf("contesto della schedule non valido: %w", err)
	}
	d := &sc.Request.Descriptor
	id := scheduledIDPrefix(d.ScheduleName()) + in.ScheduledTime.UTC().Format("20060102T150405Z")
	// I deploy della schedule risultano richiesti da chi l'ha creata. Se l'ambiente richiedeva
	// approvazioni alla creazione della schedule, ogni esecuzione va approvata.
	if sc.Request.Approval != nil {
		if err := enqueueApproval(ctx, sc.QueueName, sc.Request, id); err != nil {
			return "", err
		}
		return id, nil
	}
	h, err := dbos.Enqueue[Result](ctx, sc.QueueName, workflowName, sc.Request, deployOptions(sc.Request, id)...)
	if err != nil {
		return "", err
	}
	return h.GetWorkflowID(), nil
}

// scheduledIDPrefix è il prefisso degli ID dei deploy generati da una schedule.
func scheduledIDPrefix(schedule string) string {
	return schedule + "@"
}

// ListSchedules elenca le schedule di deploy.
func (e *Engine) ListSchedules(_ context.Context) ([]ScheduleInfo, error) {
	ss, err := dbos.ListSchedules(e.client, dbos.WithScheduleWorkflowNames(scheduledWorkflowName))
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleInfo, 0, len(ss))
	for _, s := range ss {
		out = append(out, toScheduleInfo(s))
	}
	return out, nil
}

// GetSchedule restituisce una schedule per nome.
func (e *Engine) GetSchedule(ctx context.Context, name string) (*ScheduleInfo, error) {
	ss, err := dbos.ListSchedules(e.client, dbos.WithScheduleWorkflowNames(scheduledWorkflowName), dbos.WithScheduleNames(name))
	if err != nil {
		return nil, err
	}
	if len(ss) == 0 {
		return nil, fmt.Errorf("%w: schedule %s", ErrNotFound, name)
	}
	info := toScheduleInfo(ss[0])
	return &info, nil
}

// PauseSchedule sospende i tick della schedule.
func (e *Engine) PauseSchedule(ctx context.Context, name string) error {
	return e.scheduleOp(ctx, name, "sospesa", dbos.PauseSchedule)
}

// ResumeSchedule riattiva una schedule sospesa.
func (e *Engine) ResumeSchedule(ctx context.Context, name string) error {
	return e.scheduleOp(ctx, name, "riattivata", dbos.ResumeSchedule)
}

// DeleteSchedule elimina la schedule; i deploy già accodati non vengono toccati.
func (e *Engine) DeleteSchedule(ctx context.Context, name string) error {
	return e.scheduleOp(ctx, name, "eliminata", dbos.DeleteSchedule)
}

// scheduleOp verifica l'esistenza della schedule ed esegue l'operazione indicata.
func (e *Engine) scheduleOp(ctx context.Context, name, verb string, op func(dbos.Client, string) error) error {
	if _, err := e.GetSchedule(ctx, name); err != nil {
		return err
	}
	if err := op(e.client, name); err != nil {
		return err
	}
	e.logger.Info("schedule di deploy "+verb, "schedule", name)
	return nil
}

// toScheduleInfo estrae dal contesto della schedule i dati del descrittore.
func toScheduleInfo(s dbos.WorkflowSchedule) ScheduleInfo {
	info := ScheduleInfo{
		Name:        s.ScheduleName,
		Cron:        strings.TrimSpace(s.Schedule),
		Timezone:    s.CronTimezone,
		Status:      string(s.Status),
		LastFiredAt: s.LastFiredAt,
	}
	if sc, err := dbos.DecodeScheduleContext[scheduleContext](dbos.ScheduledWorkflowInput{Context: s.Context}); err == nil {
		info.Application = sc.Request.Descriptor.Application
		info.Environment = sc.Request.Descriptor.Environment
		info.RequestedBy = sc.Request.RequestedBy
	}
	return info
}
