package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// TestCoordinator_DispatchAndAdvance_NoLockerField documents that the
// Coordinator struct does not have a Locker field — locking is fully owned by
// the workflow service. The test exercises DispatchAndAdvance directly to
// confirm the coordinator runs to completion without any lock acquisition.
func TestCoordinator_DispatchAndAdvance_NoLockerField(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
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
		{ID: "hook-a"},
	})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected done, got %q", result.NewStatus)
	}
}

// TestCoordinator_DispatchAndAdvance_NilLockerOK verifies that the coordinator
// works correctly when no external locker is involved (nil-safe path).
func TestCoordinator_DispatchAndAdvance_NilLockerOK(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"done"}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
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
		{ID: "hook-a"},
	})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch with no locker: %v", err)
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected done, got %q", result.NewStatus)
	}
}
