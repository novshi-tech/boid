package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_DispatchAndAdvance_HooksSequential(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"result-a"}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"pr":"http://example.com"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
		{ID: "hook-b", On: orchestrator.OnValues{"executing"}},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["prompt"]; !ok {
		t.Error("expected prompt in final payload")
	}

	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected new status done, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_NoAdvanceWhenConditionNotMet(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a", On: orchestrator.OnValues{"executing"}},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if result.NewStatus != "" {
		t.Errorf("expected empty new status, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_GatesExecuteAfterHooks(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)
	mock.setGateCompletion("gate-push", `{"payload_patch":{"pr":"http://pr-url"}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(
		[]projectspec.Hook{{ID: "hook-a", On: orchestrator.OnValues{"executing"}}},
		[]projectspec.Gate{{ID: "gate-push", On: orchestrator.OnValues{"executing"}}},
	)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}

	if len(mock.execOrder) != 2 {
		t.Fatalf("expected 2 executions, got %d", len(mock.execOrder))
	}
	if mock.execOrder[0] != "hook:hook-a" {
		t.Errorf("expected hook first, got %s", mock.execOrder[0])
	}
	if mock.execOrder[1] != "gate:gate-push" {
		t.Errorf("expected gate second, got %s", mock.execOrder[1])
	}
}

func TestCoordinator_DispatchAndAdvance_EmptyHooksAndGates(t *testing.T) {
	mock := newMockExecutorWaiter()
	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{}
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(result.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result.Results))
	}
}
