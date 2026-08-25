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
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
			Payload:  json.RawMessage(`{}`),
		},
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
		{ID: "hook-b"},
	})
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
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
			Payload:  json.RawMessage(`{}`),
		},
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
	})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if result.NewStatus != "" {
		t.Errorf("expected empty new status, got %q", result.NewStatus)
	}
}

// readonly task の parallel hook 経路で、 hook が exit code != 0 で死んだ場合
// に DispatchAndAdvance がエラーを返し、 lifecycle.executed=true による誤った
// auto-advance を許さないことを検証する。 修正前は dispatchParallel が ExitCode
// を集約せず err=nil を返してしまい、 lifecycle.executed=true 由来で done に
// 進んでしまっていた。
func TestCoordinator_DispatchAndAdvance_ReadonlyHookFailure_DoesNotAdvance(t *testing.T) {
	mock := newMockExecutorWaiter()
	jobID := mock.setHookCompletion("hook-a", "", 1)
	_ = jobID

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
			Readonly: true, // → IsReadonly=true → dispatchParallel 経路
			Payload:  json.RawMessage(`{}`),
		},
	}
	meta := metaWithBehavior([]projectspec.Hook{
		{ID: "hook-a"},
	})
	sm := lifecycleExecutedStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatalf("expected error from failed hook, got nil (NewStatus=%q)", result.NewStatus)
	}
	if result == nil {
		t.Fatalf("expected non-nil result for FiredEvents persistence, got nil")
	}
	if result.NewStatus != "" {
		t.Errorf("expected empty NewStatus when hook fails, got %q", result.NewStatus)
	}
	if len(result.FiredEvents) != 1 {
		t.Errorf("expected 1 FiredEvent (failed hook), got %d", len(result.FiredEvents))
	}
}

func TestCoordinator_DispatchAndAdvance_EmptyHooks(t *testing.T) {
	mock := newMockExecutorWaiter()
	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{
			Behavior: "dev",
			Payload:  json.RawMessage(`{}`),
		},
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

// TestCoordinator_DispatchAndAdvance_CardTask_ReturnsErrorNotPanic pins the
// card-model-cleanup PR-2 review finding: DispatchAndAdvance used to
// dereference task.Exec.Payload unconditionally, so a card task (Exec == nil
// by construction) reaching here — as one did in production via
// workflow_action.go's post-commit dispatch goroutine, which gated the
// launch on `s.Coordinator != nil` alone rather than also checking
// `newTask.Exec != nil` — nil-panicked. That goroutine has no recover(), so
// the panic crashed the whole daemon process, not just the request. The
// fix is two-layered: the real gate lives at the workflow_action.go call
// site (see internal/api's TestApplyAction_Park_WithRealCoordinator_
// DoesNotPanicDispatchGoroutine, which exercises that seam directly), and
// this defense-in-depth guard inside DispatchAndAdvance itself turns any
// future missed call-site gate into an error return instead of a second
// crash — mirroring AdvanceFull's pre-existing `task.Exec == nil` guard.
func TestCoordinator_DispatchAndAdvance_CardTask_ReturnsErrorNotPanic(t *testing.T) {
	coord := &orchestrator.Coordinator{}

	task := &orchestrator.Task{
		ID:        "card-1",
		Type:      orchestrator.TaskTypeCard,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusWorking,
		Card:      &orchestrator.CardAttrs{},
	}
	meta := &projectspec.ProjectMeta{}
	sm := orchestrator.NewCardMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("DispatchAndAdvance() on a card task: error = nil, want a non-nil error (task.Exec is nil by construction for a card)")
	}
	if result != nil {
		t.Fatalf("DispatchAndAdvance() on a card task: result = %+v, want nil alongside the error", result)
	}
}
