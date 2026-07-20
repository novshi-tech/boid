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
// payload` broker RPCs can serve the exact same env/payload data
// contextFiles/buildEnvironmentYAML already write to the sandbox's
// $HOME/.boid/context files — without re-deriving job-scoped facts
// (allowed_domains + resolved host commands, the trait-filtered payload)
// that only exist at dispatch time.

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
