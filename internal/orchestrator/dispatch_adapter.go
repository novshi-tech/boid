package orchestrator

import (
	"context"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/projectspec"
)

type JobDispatcher interface {
	Dispatch(ctx context.Context, plan *dispatcher.DispatchPlan) (string, error)
	WaitForJobCtx(ctx context.Context, jobID string) (dispatcher.JobCompletionResult, error)
}

// DispatchAdapter adapts a job dispatcher to hook/gate execution interfaces.
type DispatchAdapter struct {
	dispatcher JobDispatcher
	planner    *DispatchPlanner
}

func NewDispatchAdapter(dispatcher JobDispatcher, planner *DispatchPlanner) *DispatchAdapter {
	return &DispatchAdapter{dispatcher: dispatcher, planner: planner}
}

func (a *DispatchAdapter) ExecuteHook(ctx context.Context, event *projectspec.HookFireEvent) (string, error) {
	plan, err := a.planner.PlanHook(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, plan)
}

func (a *DispatchAdapter) ExecuteGate(ctx context.Context, event *projectspec.GateFireEvent) (string, error) {
	plan, err := a.planner.PlanGate(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, plan)
}

func (a *DispatchAdapter) WaitForJob(ctx context.Context, jobID string) (JobCompletion, error) {
	result, err := a.dispatcher.WaitForJobCtx(ctx, jobID)
	return JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}

type DBProjectCatalog struct {
	DB *db.DB
}

func (c DBProjectCatalog) GetProject(id string) (*projectspec.Project, error) {
	return project.GetProject(c.DB.Conn, id)
}

func (c DBProjectCatalog) ListProjects() ([]*projectspec.Project, error) {
	return project.ListProjects(c.DB.Conn)
}

type DBTaskLookup struct {
	DB *db.DB
}

func (l DBTaskLookup) GetTask(id string) (*Task, error) {
	return GetTask(l.DB.Conn, id)
}
