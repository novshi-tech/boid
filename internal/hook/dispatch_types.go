package hook

import (
	"context"
	"encoding/json"

	"github.com/novshi-tech/boid/internal/model"
)

// JobCompletion represents the result of a completed job.
type JobCompletion struct {
	JobID    string
	Output   string // stdout capture (payload_patch JSON)
	ExitCode int
}

// HookExecutor launches a hook and returns the job ID.
type HookExecutor interface {
	ExecuteHook(ctx context.Context, event *model.HookFireEvent) (jobID string, err error)
}

// GateExecutor launches a gate and returns the job ID.
type GateExecutor interface {
	ExecuteGate(ctx context.Context, event *model.GateFireEvent) (jobID string, err error)
}

// JobWaiter waits for a job to complete.
type JobWaiter interface {
	WaitForJob(ctx context.Context, jobID string) (JobCompletion, error)
}

// HandlerResult is the result of a single hook or gate execution.
type HandlerResult struct {
	ID           string          // hook or gate ID
	Role         model.Role
	ExitCode     int
	PayloadPatch json.RawMessage
}

// DispatchResult is the accumulated result of a full dispatch cycle.
type DispatchResult struct {
	Results      []HandlerResult
	FinalPayload json.RawMessage
	NewStatus    model.TaskStatus // set if orchestrator advanced the state
}
