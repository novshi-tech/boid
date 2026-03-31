package server

import (
	"context"

	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// runnerAdapter adapts job.Runner to orchestrator.HookExecutor, orchestrator.GateExecutor, and orchestrator.JobWaiter.
type runnerAdapter struct {
	runner *job.Runner
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
