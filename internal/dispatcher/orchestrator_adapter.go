package dispatcher

import (
	"context"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type dispatchBackend interface {
	Dispatch(ctx context.Context, spec *orchestrator.JobSpec, cleanup orchestrator.CleanupFunc) (string, error)
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
	spec, cleanup, err := a.planner.PlanHook(event)
	if err != nil {
		return "", err
	}
	return a.dispatcher.Dispatch(ctx, spec, cleanup)
}

func (a *OrchestratorAdapter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	result, err := a.dispatcher.WaitForJobCtx(ctx, jobID)
	return orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}
