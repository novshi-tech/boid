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
					{ID: "regular-gate", On: orchestrator.OnValues{"executing"}},
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

	// gate のみ実行の場合（hook なし）は execution_complete は注入されない
	if result.NewStatus != "" {
		t.Errorf("expected no advance for gate-only execution with empty output, got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_ExecutionComplete_InjectedOnExitZero(t *testing.T) {
	// hook が exit 0 で完了（成果物なし）→ execution_complete=true が注入され done に遷移
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
		{ID: "main-hook", On: orchestrator.OnValues{"executing"}},
	}, nil)
	sm := orchestrator.DefaultMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if !orchestrator.TraitBool(result.FinalPayload, "execution_complete") {
		t.Error("expected execution_complete=true in final payload after exit 0")
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("expected new status done (via empty result), got %q", result.NewStatus)
	}
}

func TestCoordinator_DispatchAndAdvance_ExecutionComplete_NotInjectedOnJobFailure(t *testing.T) {
	// hook が exit 1 で失敗 → error が返され execution_complete は注入されない
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
		{ID: "main-hook", On: orchestrator.OnValues{"executing"}},
	}, nil)
	sm := orchestrator.DefaultMachine()

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("expected error for failed job (exit code 1)")
	}
}
