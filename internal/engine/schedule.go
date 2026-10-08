package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// ScheduleService gestisce le schedule dei deploy ricorrenti.
type ScheduleService interface {
	ListSchedules(ctx context.Context) ([]ScheduleInfo, error)
	GetSchedule(ctx context.Context, name string) (*ScheduleInfo, error)
	PauseSchedule(ctx context.Context, name string) error
	ResumeSchedule(ctx context.Context, name string) error
	DeleteSchedule(ctx context.Context, name string) error
}

// ScheduleInfo descrive una schedule ricorrente.
type ScheduleInfo struct {
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	Timezone    string     `json:"timezone,omitempty"`
	Status      string     `json:"status"`
	Application string     `json:"application"`
	Environment string     `json:"environment"`
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
}

// applySchedule crea o aggiorna (upsert per nome) la schedule DBOS.
// Le esecuzioni sono instradate sulla coda dei deploy, quindi ne rispettano la concorrenza.
func (e *Engine) applySchedule(req Request) (*Submission, error) {
	d := req.Descriptor
	name := d.ScheduleName()
	err := dbos.ApplySchedules(e.dbos, []dbos.ScheduleSpec{{
		ScheduleName: name,
		WorkflowName: scheduledWorkflowName,
		Schedule:     d.Schedule.CronExpr(),
		CronTimezone: d.Schedule.Timezone,
		Context:      req,
		QueueName:    e.queue.GetName(),
	}})
	if err != nil {
		return nil, err
	}
	e.logger.Info("schedule di deploy applicata", "schedule", name, "cron", d.Schedule.CronExpr(), "application", d.Application, "environment", d.Environment)
	return &Submission{State: StateScheduled, Schedule: name}, nil
}

// scheduledWorkflow è invocato da DBOS a ogni tick della schedule: accoda un deploy
// con ID deterministico "<schedule>@<orario tick>", così un tick non produce mai due deploy.
// Non attende il deploy, per non occupare uno slot della coda.
func (e *Engine) scheduledWorkflow(ctx dbos.Context, in dbos.ScheduledWorkflowInput) (string, error) {
	req, err := dbos.DecodeScheduleContext[Request](in)
	if err != nil {
		return "", fmt.Errorf("contesto della schedule non valido: %w", err)
	}
	id := scheduledIDPrefix(req.Descriptor.ScheduleName()) + in.ScheduledTime.UTC().Format("20060102T150405Z")
	h, err := dbos.RunWorkflow(ctx, e.deployWorkflow, req, dbos.WithQueue(e.queue), dbos.WithWorkflowID(id))
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
	ss, err := dbos.ListSchedules(e.dbos, dbos.WithScheduleWorkflowNames(scheduledWorkflowName))
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
	ss, err := dbos.ListSchedules(e.dbos, dbos.WithScheduleWorkflowNames(scheduledWorkflowName), dbos.WithScheduleNames(name))
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
	if err := op(e.dbos, name); err != nil {
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
	if req, err := dbos.DecodeScheduleContext[Request](dbos.ScheduledWorkflowInput{Context: s.Context}); err == nil {
		info.Application = req.Descriptor.Application
		info.Environment = req.Descriptor.Environment
	}
	return info
}
