package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_DispatchAndAdvance_GateExitZeroEmptyOutput_NoArtifactInjected(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("regular-gate", "", 0) // exit 0, empty output

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
		Behavior:  "custom-behavior",
		Payload:   json.RawMessage(`{}`),
	}
	meta := &projectspec.ProjectMeta{
		TaskBehaviors: map[string]projectspec.TaskBehavior{
			"custom-behavior": {
				Name: "custom-behavior",
				Gates: []projectspec.Gate{
					{ID: "regular-gate"},
				},
			},
		},
	}
	sm := orchestrator.DefaultMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["artifact"]; ok {
		t.Error("expected no artifact injection for gate with empty output")
	}

	// gate のみ実行の場合（hook なし）は lifecycle.executed が立たず遷移しない
	if result.NewStatus != "" {
		t.Errorf("expected no advance for gate-only execution with empty output, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_LifecycleExecuted_OnExitZero(t *testing.T) {
	// hook が exit 0 で完了（成果物なし）→ lifecycle.executed=true が transient に set され done に遷移
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", `{"payload_patch":{}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
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
		{ID: "main-hook"},
	}, nil)
	sm := orchestrator.DefaultMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// lifecycle は FinalPayload に永続化されない
	if orchestrator.TraitBool(result.FinalPayload, "lifecycle.executed") {
		t.Error("expected lifecycle.executed to NOT be in persisted FinalPayload")
	}
	// lifecycle.executed が state machine に渡った結果 done に遷移する
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected new status done (via lifecycle.executed), got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_LifecycleExecuted_NotSetOnJobFailure(t *testing.T) {
	// hook が exit 1 で失敗 → error が返され lifecycle.executed は立たない
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("main-hook", ``, 1) // exit code 1

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
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
		{ID: "main-hook"},
	}, nil)
	sm := orchestrator.DefaultMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("expected error for failed job (exit code 1)")
	}
}
