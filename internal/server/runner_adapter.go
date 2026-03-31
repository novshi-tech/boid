package server

import (
	"context"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/project"
)

// runnerAdapter adapts dispatcher.Runner to orchestrator.HookExecutor, orchestrator.GateExecutor, and orchestrator.JobWaiter.
type runnerAdapter struct {
	runner *dispatcher.Runner
}

func (a *runnerAdapter) ExecuteHook(ctx context.Context, event *project.HookFireEvent) (string, error) {
	return a.runner.ExecuteHook(ctx, event)
}

func (a *runnerAdapter) ExecuteGate(ctx context.Context, event *project.GateFireEvent) (string, error) {
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
