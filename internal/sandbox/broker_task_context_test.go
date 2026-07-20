package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): the broker
// authorizes the four new task-context ops the same way it authorizes
// BoidOpJobDone/BoidOpAgentStop — defaulting an empty id from the token's
// own context, then rejecting a caller-supplied id that doesn't match. This
// is intentionally *stricter* than BoidOpTaskGet's default-only fill-in
// (which never rejects a caller-supplied TaskID that mismatches the token):
// the shim never lets an agent target another job's context (no
// `--task <id>` flag — id always comes from BOID_TASK_ID/BOID_JOB_ID env),
// so the extra equality check closes an otherwise pointless attack surface
// (a compromised in-sandbox process reading a sibling task's context) at
// zero cost to legitimate use.

func testTaskContextBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpTaskCurrent):      {},
			string(sandbox.BoidOpTaskInstructions): {},
			string(sandbox.BoidOpTaskEnv):          {},
			string(sandbox.BoidOpTaskPayload):      {},
		}},
	}
}

func registerTaskContextToken(broker *sandbox.Broker, projectDir string) string {
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	return broker.Register(map[string]sandbox.CommandDef{}, testTaskContextBoidPolicies(), ctx)
}

func TestBroker_BoidBuiltinTaskCurrent_DefaultsAndRestrictsTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskContextToken(broker, projectDir)

	// Empty TaskID defaults from the token context and reaches the executor.
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCurrent},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].TaskID != "task-keep" {
		t.Fatalf("expected executor call with task-keep, got %+v", exec.calls)
	}

	// A mismatched explicit TaskID is rejected before reaching the executor.
	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCurrent, TaskID: "other-task"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current task") {
		t.Fatalf("expected task id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-task request, calls=%d", len(exec.calls))
	}
}

// BoidOpTaskInstructions is JobID-scoped, not TaskID-scoped (codex review on
// PR #797: two agent-kind hooks for different agents can be dispatched from
// the same task in one evaluation round, so a TaskID-only guard would let a
// claude job read a codex job's instructions as long as they shared a task
// — see wiring-seams.md #13). A mismatched TaskID alone must NOT be
// rejected (there is no TaskID-equality check for this op at all); only a
// mismatched JobID is.
func TestBroker_BoidBuiltinTaskInstructions_DefaultsAndRestrictsJobID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskContextToken(broker, projectDir)

	// Empty JobID defaults from the token context and reaches the executor.
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskInstructions},
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
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskInstructions, JobID: "other-job"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-job request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidBuiltinTaskEnv_DefaultsAndRestrictsJobID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskContextToken(broker, projectDir)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskEnv},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].JobID != "job-keep" {
		t.Fatalf("expected executor call with job-keep, got %+v", exec.calls)
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskEnv, JobID: "other-job"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-job request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidBuiltinTaskPayload_SameGuard(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskContextToken(broker, projectDir)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskPayload, JobID: "other-job"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current job") {
		t.Fatalf("expected job id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// A token with no "boid" builtin policy attached at all is rejected before
// any op-specific logic runs, same as every existing boid builtin op.
func TestBroker_BoidBuiltinTaskCurrent_RequiresBoidPolicy(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", ProjectDir: projectDir}
	token := broker.Register(map[string]sandbox.CommandDef{}, nil, ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCurrent},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "command not allowed") {
		t.Fatalf("expected 'command not allowed', got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// A "boid" policy that does not list the op is rejected too (op-level
// authorization, distinct from the coarser "has boid policy at all" check
// above).
func TestBroker_BoidBuiltinTaskEnv_RejectsWhenOpNotInPolicy(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", ProjectDir: projectDir}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskEnv},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed by policy") {
		t.Fatalf("expected 'not allowed by policy', got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}
