package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR2 (docs/plans/phase5-shim-and-task-context.md): the broker
// authorizes `boid task attachments list` / `get <name>` the same way it
// authorizes `boid task current` — attachments belong to the *task* (not a
// specific job — any job dispatched from the task may read them), so the
// guard defaults an empty TaskID from the token's own context and then
// rejects a caller-supplied TaskID that mismatches it. The shim never lets
// an agent target another task's attachments (no `--task <id>` flag — id
// always comes from BOID_TASK_ID env), so the extra equality check closes
// an otherwise pointless cross-task read at zero cost to legitimate use.

func testTaskAttachmentsBoidPolicies() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpTaskAttachmentsList): {},
			string(sandbox.BoidOpTaskAttachmentsGet):  {},
		}},
	}
}

func registerTaskAttachmentsToken(broker *sandbox.Broker, projectDir string) string {
	ctx := sandbox.TokenContext{
		JobID:      "job-keep",
		TaskID:     "task-keep",
		ProjectID:  "proj-keep",
		Role:       testRoleHook,
		ProjectDir: projectDir,
	}
	return broker.Register(map[string]sandbox.CommandDef{}, testTaskAttachmentsBoidPolicies(), ctx)
}

func TestBroker_BoidBuiltinTaskAttachmentsList_DefaultsAndRestrictsTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskAttachmentsToken(broker, projectDir)

	// Empty TaskID defaults from the token context and reaches the executor.
	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsList},
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
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsList, TaskID: "other-task"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current task") {
		t.Fatalf("expected task id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-task request, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidBuiltinTaskAttachmentsGet_DefaultsAndRestrictsTaskID(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskAttachmentsToken(broker, projectDir)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet, AttachmentName: "shot.png"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 || exec.calls[0].TaskID != "task-keep" || exec.calls[0].AttachmentName != "shot.png" {
		t.Fatalf("expected executor call with task-keep/shot.png, got %+v", exec.calls)
	}

	resp = broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet, TaskID: "other-task", AttachmentName: "shot.png"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "restricted to the current task") {
		t.Fatalf("expected task id rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor should not receive the cross-task request, calls=%d", len(exec.calls))
	}
}

// BoidOpTaskAttachmentsGet additionally requires a non-empty AttachmentName
// — unlike List, which has no per-item argument at all.
func TestBroker_BoidBuiltinTaskAttachmentsGet_RequiresAttachmentName(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := registerTaskAttachmentsToken(broker, projectDir)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "requires an attachment name") {
		t.Fatalf("expected attachment name requirement error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not receive a request with no attachment name, calls=%d", len(exec.calls))
	}
}

// A token with no "boid" builtin policy attached at all is rejected before
// any op-specific logic runs, same as every existing boid builtin op.
func TestBroker_BoidBuiltinTaskAttachmentsList_RequiresBoidPolicy(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", ProjectDir: projectDir}
	token := broker.Register(map[string]sandbox.CommandDef{}, nil, ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsList},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "command not allowed") {
		t.Fatalf("expected 'command not allowed', got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// A "boid" policy that does not list the op is rejected too (op-level
// authorization, distinct from the coarser "has boid policy at all" check
// above).
func TestBroker_BoidBuiltinTaskAttachmentsGet_RejectsWhenOpNotInPolicy(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	ctx := sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", ProjectDir: projectDir}
	token := broker.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), ctx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpTaskAttachmentsGet, AttachmentName: "shot.png"},
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "not allowed by policy") {
		t.Fatalf("expected 'not allowed by policy', got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}
