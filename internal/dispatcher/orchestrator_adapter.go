package dispatcher

import (
	"context"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type dispatchBackend interface {
	Dispatch(ctx context.Context, request *orchestrator.JobSpec) (string, error)
	WaitForJobCtx(ctx context.Context, jobID string) (JobCompletionResult, error)
}

// OrchestratorAdapter adapts dispatcher execution to orchestrator interfaces.
type OrchestratorAdapter struct {
	dispatcher dispatchBackend
	planner    *orchestrator.DispatchPlanner
}

func NewOrchestratorAdapter(dispatcher dispatchBackend, planner *orchestrator.DispatchPlanner) *OrchestratorAdapter {
	return &OrchestratorAdapter{dispatcher: dispatcher, planner: planner}
}

func (a *OrchestratorAdapter) ExecuteHook(ctx context.Context, event *orchestrator.HookFireEvent) (string, error) {
	request, err := a.planner.PlanHook(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, request)
}

func (a *OrchestratorAdapter) ExecuteGate(ctx context.Context, event *orchestrator.GateFireEvent) (string, error) {
	request, err := a.planner.PlanGate(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, request)
}

func (a *OrchestratorAdapter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	result, err := a.dispatcher.WaitForJobCtx(ctx, jobID)
	return orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}
