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
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "main-hook"}}, nil)
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

func TestCoordinator_ReplayHook_ExitGateChain(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", `{"payload_patch":{"step":"hooked"}}`, 0)
	mock.setGateCompletion("check-gate", `{"payload_patch":{"prompt":"done"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(
		[]projectspec.Hook{{ID: "main-hook"}},
		[]projectspec.Gate{{ID: "check-gate", Phase: projectspec.GatePhaseExit}},
	)
	sm := simpleStateMachine() // advances to done when "prompt" key exists

	result, err := coord.ReplayHook(context.Background(), task, meta, sm, "main-hook")
	if err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}

	// hook fired first, then exit gate
	if len(mock.execOrder) != 2 || mock.execOrder[0] != "hook:main-hook" || mock.execOrder[1] != "gate:check-gate" {
		t.Fatalf("exec order = %v, want [hook:main-hook gate:check-gate]", mock.execOrder)
	}

	// gate patch provided "prompt" → advance to done
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("NewStatus = %q, want done", result.NewStatus)
	}

	// FiredEvents: 1 hook_replay + 1 exit_gate
	if len(result.FiredEvents) != 2 {
		t.Fatalf("FiredEvents = %d, want 2", len(result.FiredEvents))
	}
	if result.FiredEvents[0].Kind != "hook_replay" {
		t.Errorf("FiredEvents[0].Kind = %q, want hook_replay", result.FiredEvents[0].Kind)
	}
	if result.FiredEvents[1].Kind != "exit_gate" {
		t.Errorf("FiredEvents[1].Kind = %q, want exit_gate", result.FiredEvents[1].Kind)
	}
}

func TestCoordinator_ReplayHook_HookNotFound(t *testing.T) {
	mock := newMockExecutorWaiter()
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:       "task-1",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "other-hook"}}, nil)

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
		GateExecutor: mock,
		Waiter:       mock,
	}

	// Hook requires executing status; task is pending → no match
	task := &orchestrator.Task{
		ID:       "task-2",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusPending,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "main-hook"}}, nil)

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
	}, nil)

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
	meta := metaWithBehavior([]projectspec.Hook{{ID: "hook-a"}}, nil)

	// Hooks only fire during executing state
	hooks := orchestrator.ListHooksForStatus(meta, task, orchestrator.TaskStatusPending)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for pending, got %d", len(hooks))
	}
}
