package server

import (
	"context"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// runnerAdapter adapts dispatcher.Runner to orchestrator.HookExecutor, orchestrator.GateExecutor, and orchestrator.JobWaiter.
type runnerAdapter struct {
	runner *dispatcher.Runner
}

func (a *runnerAdapter) ExecuteHook(ctx context.Context, event *model.HookFireEvent) (string, error) {
	return a.runner.ExecuteHook(ctx, event)
}

func (a *runnerAdapter) ExecuteGate(ctx context.Context, event *model.GateFireEvent) (string, error) {
	return a.runner.ExecuteGate(ctx, event)
}

func (a *runnerAdapter) WaitForJob(ctx context.Context, jobID string) (orchestrator.JobCompletion, error) {
	result, err := a.runner.WaitForJobCtx(ctx, jobID)
	return orchestrator.JobCompletion{
		JobID:    jobID,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, err
}

