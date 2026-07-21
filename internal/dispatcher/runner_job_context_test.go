package dispatcher_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): Runner tracks a
// JobContextSnapshot per dispatched job so the `boid task env` / `boid task
// payload` broker RPCs can serve this exact job's env/payload data — without
// re-deriving job-scoped facts (allowed_domains + resolved host commands,
// the trait-filtered payload) that only exist at dispatch time.

func TestDispatch_TracksJobContext_EnvAndPayload(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()
	r.AllowedDomains = []string{"github.com", "example.com"}

	payload := json.RawMessage(`{"artifact":{"report":"ok"}}`)
	spec := &orchestrator.JobSpec{
		ProjectID:    "proj-1",
		Argv:         []string{"echo", "hi"},
		Kind:         orchestrator.JobKindHook,
		PrimaryInput: payload,
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {Name: "gh", AllowedSubcommands: []string{"pr"}},
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found after successful Dispatch", jobID)
	}
	if len(snap.Env.AllowedDomains) != 2 || snap.Env.AllowedDomains[0] != "github.com" {
		t.Errorf("Env.AllowedDomains = %v, want [github.com example.com]", snap.Env.AllowedDomains)
	}
	if len(snap.Env.HostCommands) != 1 || snap.Env.HostCommands[0].Name != "gh" {
		t.Errorf("Env.HostCommands = %+v, want 1 entry named gh", snap.Env.HostCommands)
	}
	if string(snap.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", snap.Payload, payload)
	}
}

// TestDispatch_TracksJobContext_Instructions_MatchesJobSpec verifies
// JobContextSnapshot.Instructions is populated straight from
// spec.Instruction — the same value contextFiles would have written to
// instructions.yaml for this exact job.
func TestDispatch_TracksJobContext_Instructions_MatchesJobSpec(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Instruction: &orchestrator.RoutedInstruction{
			Agent:   "claude-code",
			Message: "do the thing",
		},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Instructions) != 1 || snap.Instructions[0].Agent != "claude-code" || snap.Instructions[0].Message != "do the thing" {
		t.Errorf("Instructions = %+v, want the single routed instruction from spec.Instruction", snap.Instructions)
	}
}

// TestDispatch_TracksJobContext_Instructions_NilJobSpecInstructionYieldsEmpty
// is the direct regression guard for the codex-review finding on PR #797:
// orchestrator.Evaluator can fire two agent-kind hooks for different agents
// from the same task in one round (extractInstructionAgents matches any
// agent in the instruction history, not just the active/last entry), but
// only the hook whose agent equals the *last* history entry gets a non-nil
// spec.Instruction (selectInstruction/FilterInstructions only look at the
// last entry) — the other hook's JobSpec.Instruction is nil, and its
// instructions.yaml file is correspondingly never written. A job whose
// spec.Instruction is nil must track an EMPTY instructions list, not
// something re-derived from the task row (which would incorrectly hand it
// the other hook's agent's instruction).
func TestDispatch_TracksJobContext_Instructions_NilJobSpecInstructionYieldsEmpty(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()

	// Simulates the claude-code hook's JobSpec when the task's instruction
	// history is [claude-code, codex] (active/last entry is codex): the
	// evaluator still matches and fires the claude-code hook (its agent
	// appears in the history), but selectInstruction returns nil for it
	// since it doesn't match the *active* (last) entry.
	spec := &orchestrator.JobSpec{
		ProjectID:   "proj-1",
		Argv:        []string{"echo", "hi"},
		Kind:        orchestrator.JobKindHook,
		Instruction: nil,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Instructions) != 0 {
		t.Errorf("Instructions = %+v, want empty (spec.Instruction was nil) — must NOT be re-derived from the task row", snap.Instructions)
	}
}

func TestDispatch_TracksJobContext_NilPrimaryInput(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindExec,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	snap, ok := r.JobContext(jobID)
	if !ok {
		t.Fatalf("JobContext(%q) not found", jobID)
	}
	if len(snap.Payload) != 0 {
		t.Errorf("Payload = %s, want empty for a job with no PrimaryInput", snap.Payload)
	}
}

func TestUnregisterJob_RemovesJobContext(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime()

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, ok := r.JobContext(jobID); !ok {
		t.Fatalf("JobContext(%q) should be tracked right after Dispatch", jobID)
	}

	r.UnregisterJob(jobID)

	if _, ok := r.JobContext(jobID); ok {
		t.Errorf("JobContext(%q) should be gone after UnregisterJob", jobID)
	}
}

func TestJobContext_UnknownJobID_ReturnsFalse(t *testing.T) {
	r := &dispatcher.Runner{}
	if _, ok := r.JobContext("no-such-job"); ok {
		t.Error("expected ok=false for an untracked job id")
	}
}
