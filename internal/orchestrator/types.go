package orchestrator

import (
	"context"
	"encoding/json"
)

// JobCompletion represents the result of a completed job.
type JobCompletion struct {
	JobID    string
	Output   string // stdout capture (the hook's own `{"payload_patch": ...}` fallback, if any)
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
	Results     []HandlerResult
	FiredEvents []FiredEvent
	// FinalPayload is the FULL payload this cycle computed: the task's
	// payload AS IT WAS AT THE START of this dispatch cycle (a snapshot that
	// can go stale the instant a long-running hook job starts, since the
	// hook itself may write to the task's payload mid-flight — e.g. via
	// `boid task update --payload-patch`/`--payload-file`), with this
	// cycle's own hook PayloadPatches merged on top. It is what
	// evaluateAndAdvance's state-machine Advance decision is based on; it
	// must NOT be used to persist the task's payload (see PayloadDelta).
	FinalPayload json.RawMessage
	// PayloadDelta is the SAME hook-patch merge as FinalPayload, but folded
	// starting from an empty object instead of the (potentially stale)
	// starting snapshot — i.e. only what THIS cycle's own hooks actually
	// wrote, never the pre-existing payload they didn't touch. Callers that
	// persist a dispatch cycle's result (runDispatchLoop, ReplayHook's
	// caller) must apply THIS to a freshly re-read task row, not
	// FinalPayload: applying the full stale snapshot on top of a fresh row
	// silently reverts any out-of-band write the hook itself made during
	// its own run (Phase 5b PR7 codex review Blocker 1, wiring-seams.md
	// #17) — e.g. a reopened task's `--payload-patch` report write getting
	// clobbered back to the pre-reopen value once the hook completes and
	// this cycle's post-hook persist runs. Empty ("{}") when no hook in
	// this cycle wrote anything, which is the common case for an agent job
	// that reports exclusively through the direct RPC paths — applying an
	// empty delta onto a fresh row is a correct no-op (orchestrator.
	// MergePayload's own empty-update short-circuit returns base
	// unchanged), so a stale snapshot never even gets a chance to overwrite
	// anything.
	PayloadDelta  json.RawMessage
	NewStatus     TaskStatus      // set if orchestrator advanced the state
	ActionPayload json.RawMessage // optional payload to attach to the auto_advance action
}
