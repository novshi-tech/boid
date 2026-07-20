package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): the executor
// dispatch tests for the four new task-context RPCs (`boid task current` /
// `instructions` / `env` / `payload`). current/instructions are backed by
// api.TaskAppService (live from the task row); env/payload are backed by a
// jobContextProvider (dispatcher.Runner's per-job JobContextSnapshot in
// production, a stub here).

type stubJobContextProvider struct {
	contexts map[string]dispatcher.JobContextSnapshot
}

func (s *stubJobContextProvider) JobContext(jobID string) (dispatcher.JobContextSnapshot, bool) {
	if s == nil {
		return dispatcher.JobContextSnapshot{}, false
	}
	snap, ok := s.contexts[jobID]
	return snap, ok
}

// --- task_current ---

func TestBoidBuiltinExecutor_TaskCurrent_HappyPath(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "task-1", ProjectID: "proj-1", Title: "hello", Status: orchestrator.TaskStatusExecuting, Behavior: "dev"},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskCurrent,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	var snap orchestrator.TaskSnapshot
	if err := json.Unmarshal([]byte(resp.Stdout), &snap); err != nil {
		t.Fatalf("stdout is not JSON: %q: %v", resp.Stdout, err)
	}
	if snap.ID != "task-1" || snap.Title != "hello" || snap.Status != "executing" || snap.Behavior != "dev" {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
}

func TestBoidBuiltinExecutor_TaskCurrent_WithField(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "task-1", ProjectID: "proj-1", Title: "hello", Status: orchestrator.TaskStatusExecuting},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskCurrent,
		TaskID:    "task-1",
		TaskField: "title",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "hello" {
		t.Errorf("stdout = %q, want plain-text %q", resp.Stdout, "hello")
	}
}

func TestBoidBuiltinExecutor_TaskCurrent_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: nil}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskCurrent,
		TaskID: "task-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskCurrent_NotFound(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: &capturingTaskStore{}}}
	ctx := sandbox.TokenContext{TaskID: "no-such-task", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskCurrent,
		TaskID: "no-such-task",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error for unknown task, got exit=%d", resp.ExitCode)
	}
}

// --- task_instructions ---

func TestBoidBuiltinExecutor_TaskInstructions_HappyPath(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{
			ID:        "task-1",
			ProjectID: "proj-1",
			Status:    orchestrator.TaskStatusExecuting,
			Instructions: orchestrator.Instructions{
				{Agent: "claude-code", Name: "dev", Message: "do it"},
			},
		},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskInstructions,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	var list []orchestrator.RoutedInstruction
	if err := json.Unmarshal([]byte(resp.Stdout), &list); err != nil {
		t.Fatalf("stdout is not a JSON array: %q: %v", resp.Stdout, err)
	}
	if len(list) != 1 || list[0].Agent != "claude-code" || list[0].Message != "do it" {
		t.Errorf("unexpected instructions: %+v", list)
	}
}

func TestBoidBuiltinExecutor_TaskInstructions_NoneReturnsEmptyArray(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "task-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusPending},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskInstructions,
		TaskID: "task-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if strings.TrimSpace(resp.Stdout) != "[]" {
		t.Errorf("stdout = %q, want empty JSON array", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskInstructions_WithField(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{
			ID:        "task-1",
			ProjectID: "proj-1",
			Status:    orchestrator.TaskStatusExecuting,
			Instructions: orchestrator.Instructions{
				{Agent: "claude-code", Name: "dev", Message: "do it"},
			},
		},
	}}
	exec := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store}}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskInstructions,
		TaskID:    "task-1",
		TaskField: "0.message",
	})
	// Numeric array indices are not object keys (ResolveJSONField's rule
	// applies here too — a []RoutedInstruction is a JSON array, not an
	// object), so this must error rather than silently resolve.
	if resp.ExitCode != 1 {
		t.Fatalf("expected error resolving an array-index field, got exit=%d stdout=%q", resp.ExitCode, resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskInstructions_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{tasks: nil}
	ctx := sandbox.TokenContext{TaskID: "task-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskInstructions,
		TaskID: "task-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

// --- task_env ---

func TestBoidBuiltinExecutor_TaskEnv_HappyPath(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {
			Env: dispatcher.WorkspaceEnvView{
				AllowedDomains: []string{"github.com"},
				HostCommands: []dispatcher.WorkspaceEnvHostCommand{
					{Name: "gh", Allow: []string{"pr"}},
				},
			},
		},
	}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskEnv,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	var view dispatcher.WorkspaceEnvView
	if err := json.Unmarshal([]byte(resp.Stdout), &view); err != nil {
		t.Fatalf("stdout is not JSON: %q: %v", resp.Stdout, err)
	}
	if len(view.AllowedDomains) != 1 || view.AllowedDomains[0] != "github.com" {
		t.Errorf("AllowedDomains = %v", view.AllowedDomains)
	}
	if len(view.HostCommands) != 1 || view.HostCommands[0].Name != "gh" {
		t.Errorf("HostCommands = %+v", view.HostCommands)
	}
}

func TestBoidBuiltinExecutor_TaskEnv_WithField(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {Env: dispatcher.WorkspaceEnvView{AllowedDomains: []string{"github.com", "example.com"}}},
	}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskEnv,
		JobID:     "job-1",
		TaskField: "allowed_domains",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != `["github.com","example.com"]` {
		t.Errorf("stdout = %q", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskEnv_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{jobContexts: nil}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskEnv,
		JobID: "job-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_TaskEnv_NoContextForJob(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-unknown", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskEnv,
		JobID: "job-unknown",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected error when no context is tracked for the job, got exit=%d", resp.ExitCode)
	}
}

// --- task_payload ---

func TestBoidBuiltinExecutor_TaskPayload_HappyPath(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {Payload: json.RawMessage(`{"artifact":{"report":"ok"}}`)},
	}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskPayload,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != `{"artifact":{"report":"ok"}}` {
		t.Errorf("stdout = %q", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskPayload_EmptyPayloadReturnsEmptyObject(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {},
	}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskPayload,
		JobID: "job-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "{}" {
		t.Errorf("stdout = %q, want empty object", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskPayload_WithField(t *testing.T) {
	provider := &stubJobContextProvider{contexts: map[string]dispatcher.JobContextSnapshot{
		"job-1": {Payload: json.RawMessage(`{"artifact":{"claude_code":{"sessions":["s1","s2"]}}}`)},
	}}
	exec := &boidBuiltinExecutor{jobContexts: provider}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskPayload,
		JobID:     "job-1",
		TaskField: "artifact.claude_code.sessions",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != `["s1","s2"]` {
		t.Errorf("stdout = %q, want the sessions array", resp.Stdout)
	}
}

func TestBoidBuiltinExecutor_TaskPayload_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{jobContexts: nil}
	ctx := sandbox.TokenContext{JobID: "job-1", ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskPayload,
		JobID: "job-1",
	})
	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "unavailable") {
		t.Fatalf("expected unavailable error, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
}
