package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_DispatchAndAdvance_ExclusiveTraitCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"prompt":"from-a"}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"prompt":"from-b"}}`, 0)

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

	_, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err == nil {
		t.Fatal("expected error for exclusive trait collision")
	}
}

func TestCoordinator_DispatchAndAdvance_SharedTraitNoCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"verification":{"findings":[{"message":"ok","status":"resolved"}]}}}`, 0)
	mock.setHookCompletion("hook-b", `{"payload_patch":{"verification":{"findings":[{"message":"bug","status":"open"}]}}}`, 0)

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
		t.Fatalf("shared trait should not collide: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	var verification map[string]json.RawMessage
	json.Unmarshal(payload["verification"], &verification)
	if _, ok := verification["hook-a"]; !ok {
		t.Error("expected hook-a sub-key in verification")
	}
	if _, ok := verification["hook-b"]; !ok {
		t.Error("expected hook-b sub-key in verification")
	}

	for _, key := range []string{"hook-a", "hook-b"} {
		var entry struct {
			SourceState string `json:"source_state"`
		}
		if err := json.Unmarshal(verification[key], &entry); err != nil {
			t.Fatalf("unmarshal %s: %v", key, err)
		}
		if entry.SourceState != "executing" {
			t.Errorf("%s: source_state = %q, want %q", key, entry.SourceState, "executing")
		}
	}
}
