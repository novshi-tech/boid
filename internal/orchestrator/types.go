package orchestrator

import (
	"context"
	"encoding/json"
)

// JobCompletion represents the result of a completed job.
type JobCompletion struct {
	JobID    string
	Output   string // stdout capture or payload_patch.json content
	ExitCode int
}

// HookExecutor launches a hook and returns the job ID.
type HookExecutor interface {
	ExecuteHook(ctx context.Context, event *HookFireEvent) (jobID string, err error)
}

// JobWaiter waits for a job to complete.
type JobWaiter interface {
	WaitForJob(ctx context.Context, jobID string) (JobCompletion, error)
}

// HandlerResult is the result of a single hook execution.
type HandlerResult struct {
	ID           string // hook ID
	Role         Role
	JobID        string // ID of the job that executed this handler
	ExitCode     int
	PayloadPatch json.RawMessage
}

// FiredEvent records a single hook execution for action logging.
type FiredEvent struct {
	KitID       string // kit that owns this handler; empty for project-local
	HandlerID   string // hook ID
	JobID       string // ID of the job that executed this handler
	Kind        string // "hook" or "hook_replay"
	SourceState string // task status at the time of dispatch
	Success     bool
	Error       string
}

// DispatchResult is the accumulated result of a full dispatch cycle.
type DispatchResult struct {
	Results       []HandlerResult
	FiredEvents   []FiredEvent
	FinalPayload  json.RawMessage
	NewStatus     TaskStatus      // set if orchestrator advanced the state
	ActionPayload json.RawMessage // optional payload to attach to the auto_advance action
}

