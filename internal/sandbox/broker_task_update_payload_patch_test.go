package sandbox_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): BoidOpTaskUpdatePayloadPatch
// is JobID-scoped, mirroring BoidOpTaskInstructions/Env/Payload (not TaskID-scoped
// like BoidOpTaskCurrent) — the merge needs to resolve the calling job's own
// HandlerID, so a mismatched or missing job id must be rejected before the
// executor ever sees the request.

func testTaskUpdatePayloadPatchPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpTaskUpdatePayloadPatch): {},
		}},
	}
}

func TestBroker_BoidTaskUpdatePayloadPatch_DefaultsAndRestrictsJobID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testTaskUpdatePayloadPatchPolicies(), ctx)

	patch := json.RawMessage(`{"artifact":{"report":{"summary":"done"}}}`)

	// Empty JobID defaults from the token context and reaches the executor.
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdatePayloadPatch, PayloadPatch: patch},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].JobID != "job-keep" {
		t.Fatalf("expected executor call with job-keep, got %+v", exec.calls)
	}

	// A mismatched explicit JobID is rejected before reaching the executor.
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdatePayloadPatch, JobID: "other-job", PayloadPatch: patch},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-job request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskUpdatePayloadPatch_RequiresPayloadPatch(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testTaskUpdatePayloadPatchPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdatePayloadPatch},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "requires a payload patch") {
		t.Fatalf("expected payload-patch-required rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive the empty-patch request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidTaskUpdatePayloadPatch_PolicyReject(t *testing.T) {
	assertBoidOpRejectedByPolicy(t, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdatePayloadPatch, JobID: "j1", PayloadPatch: json.RawMessage(`{}`)})
}

// TestBroker_BoidTaskUpdatePayloadPatch_RejectsOversizedPatch pins the
// broker-side half of the Phase 5b PR7 codex review Major 3 defense in
// depth (wiring-seams.md #17): the shim already caps --payload-patch's
// content before ever sending the request, but the broker re-checks
// PayloadPatchMaxBytes independently so a shim bypass (or a future, less
// careful caller building the request by hand) still can't push an
// oversized patch through to the executor.
func TestBroker_BoidTaskUpdatePayloadPatch_RejectsOversizedPatch(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	token := broker.Register(map[string]sandbox.CommandDef{}, testTaskUpdatePayloadPatchPolicies(), ctx)

	oversized := make(json.RawMessage, sandbox.PayloadPatchMaxBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdatePayloadPatch, PayloadPatch: oversized},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "exceeds") {
		t.Fatalf("expected oversized-patch rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive the oversized request, calls=%d", len(exec.calls))
	}
}
