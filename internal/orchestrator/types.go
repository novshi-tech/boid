package orchestrator

import (
	"context"
	"encoding/json"
)

// JobCompletion represents the result of a completed job.
type JobCompletion struct {
	JobID    string
	Output   string // stdout capture or payload_patch.yaml content
	ExitCode int
}

// HookExecutor launches a hook and returns the job ID.
type HookExecutor interface {
	ExecuteHook(ctx context.Context, event *HookFireEvent) (jobID string, err error)
}

// GateExecutor launches a gate and returns the job ID.
type GateExecutor interface {
	ExecuteGate(ctx context.Context, event *GateFireEvent) (jobID string, err error)
}

// JobWaiter waits for a job to complete.
type JobWaiter interface {
	WaitForJob(ctx context.Context, jobID string) (JobCompletion, error)
}

// HandlerResult is the result of a single hook or gate execution.
type HandlerResult struct {
	ID           string // hook or gate ID
	Role         Role
	ExitCode     int
	PayloadPatch json.RawMessage
}

// FiredEvent records a single hook or gate execution for action logging.
type FiredEvent struct {
	KitID       string // kit that owns this handler; empty for project-local
	HandlerID   string // hook or gate ID
	Kind        string // "hook", "exit_gate", or "entry_gate"
	SourceState string // task status at the time of dispatch
	Success     bool
	Error       string
}

// DispatchResult is the accumulated result of a full dispatch cycle.
type DispatchResult struct {
	Results      []HandlerResult
	FiredEvents  []FiredEvent
	FinalPayload json.RawMessage
	NewStatus    TaskStatus // set if orchestrator advanced the state
}

// EntryGateResult holds the output of entry-phase gate dispatch.
// Unlike DispatchResult it carries no NewStatus — entry gates only produce payload patches.
type EntryGateResult struct {
	Results      []HandlerResult
	FiredEvents  []FiredEvent
	FinalPayload json.RawMessage
}
