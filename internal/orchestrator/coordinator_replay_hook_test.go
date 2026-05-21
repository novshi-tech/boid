package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_ReplayHook_SingleHook(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", `{"payload_patch":{"step":"replayed"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "main-hook"}})
	sm := simpleStateMachine()

	result, err := coord.ReplayHook(context.Background(), task, meta, sm, "main-hook")
	if err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &payload); err != nil {
		t.Fatalf("unmarshal final payload: %v", err)
	}
	if string(payload["step"]) != `"replayed"` {
		t.Errorf("step = %s, want \"replayed\"", payload["step"])
	}

	if len(result.FiredEvents) != 1 {
		t.Fatalf("FiredEvents = %d, want 1", len(result.FiredEvents))
	}
	if result.FiredEvents[0].Kind != "hook_replay" {
		t.Errorf("FiredEvent.Kind = %q, want hook_replay", result.FiredEvents[0].Kind)
	}
}

func TestCoordinator_ReplayHook_HookNotFound(t *testing.T) {
	mock := newMockExecutorWaiter()
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:       "task-1",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "other-hook"}})

	_, err := coord.ReplayHook(context.Background(), task, meta, simpleStateMachine(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent hook, got nil")
	}
}

func TestCoordinator_ReplayHook_StatusMismatch(t *testing.T) {
	mock := newMockExecutorWaiter()
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		Waiter:       mock,
	}

	// Hook requires executing status; task is pending → no match
	task := &orchestrator.Task{
		ID:       "task-2",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusPending,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "main-hook"}})

	_, err := coord.ReplayHook(context.Background(), task, meta, simpleStateMachine(), "main-hook")
	if err == nil {
		t.Fatal("expected error when hook does not match status, got nil")
	}
}

func TestListHooksForStatus_ReturnsMatchingHooks(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-1",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
		{ID: "hook-b"},
	})

	hooks := orchestrator.ListHooksForStatus(meta, task, orchestrator.TaskStatusExecuting)
	if len(hooks) != 2 {
		t.Fatalf("expected 2 hooks for executing, got %d", len(hooks))
	}
}

func TestListHooksForStatus_NonExecutingReturnsNone(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-2",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "hook-a"}})

	// Hooks only fire during executing state
	hooks := orchestrator.ListHooksForStatus(meta, task, orchestrator.TaskStatusPending)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for pending, got %d", len(hooks))
	}
}
