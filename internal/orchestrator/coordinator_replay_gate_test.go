package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// replayGateStateMachine is a state machine with a verifying→done transition
// triggered by a "passed" key in the payload.
func replayGateStateMachine() *orchestrator.StateMachine {
	return &orchestrator.StateMachine{
		Name: "replay-test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{Action: "reopen", FromStatus: "done", ToStatus: "reworking"},
			{Action: "abort", FromStatus: "*", ToStatus: "aborted"},
			{
				FromStatus: "verifying",
				ToStatus:   "done",
				Condition: func(p json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(p, &m)
					v, ok := m["passed"]
					return ok && string(v) == "true"
				},
			},
		},
	}
}

func TestCoordinator_ReplayGate_ExitGate_Basic(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("check-gate", `{"payload_patch":{"passed":true}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "task-replay-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusVerifying,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "check-gate", On: orchestrator.OnValues{"verifying"}, Phase: projectspec.GatePhaseExit},
	})
	sm := replayGateStateMachine()

	result, err := coord.ReplayGate(context.Background(), task, meta, sm, "check-gate")
	if err != nil {
		t.Fatalf("ReplayGate() error = %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["passed"]; !ok {
		t.Error("expected 'passed' in final payload")
	}
	if result.NewStatus != orchestrator.TaskStatusDone {
		t.Errorf("NewStatus = %q, want %q", result.NewStatus, orchestrator.TaskStatusDone)
	}
	if len(result.FiredEvents) != 1 {
		t.Fatalf("FiredEvents = %d, want 1", len(result.FiredEvents))
	}
	if result.FiredEvents[0].Kind != "gate_replay" {
		t.Errorf("FiredEvent.Kind = %q, want %q", result.FiredEvents[0].Kind, "gate_replay")
	}
}

func TestCoordinator_ReplayGate_ExitGate_NoAdvanceWhenConditionNotMet(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("check-gate", `{"payload_patch":{}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "task-replay-2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusVerifying,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "check-gate", On: orchestrator.OnValues{"verifying"}, Phase: projectspec.GatePhaseExit},
	})
	sm := replayGateStateMachine()

	result, err := coord.ReplayGate(context.Background(), task, meta, sm, "check-gate")
	if err != nil {
		t.Fatalf("ReplayGate() error = %v", err)
	}
	if result.NewStatus != "" {
		t.Errorf("NewStatus = %q, want empty (no advance)", result.NewStatus)
	}
}

func TestCoordinator_ReplayGate_EntryGate_NoAdvance(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("setup-gate", `{"payload_patch":{"initialized":true}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "task-replay-3",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "setup-gate", On: orchestrator.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry},
	})
	sm := replayGateStateMachine()

	result, err := coord.ReplayGate(context.Background(), task, meta, sm, "setup-gate")
	if err != nil {
		t.Fatalf("ReplayGate() error = %v", err)
	}
	// Entry gates must never trigger advance.
	if result.NewStatus != "" {
		t.Errorf("entry gate replay must not advance, got NewStatus = %q", result.NewStatus)
	}
	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["initialized"]; !ok {
		t.Error("expected 'initialized' in final payload")
	}
}

func TestCoordinator_ReplayGate_GateNotFound(t *testing.T) {
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: newMockExecutorWaiter(),
		GateExecutor: newMockExecutorWaiter(),
		Waiter:       newMockExecutorWaiter(),
	}

	task := &orchestrator.Task{
		ID:        "task-replay-4",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusVerifying,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "real-gate", On: orchestrator.OnValues{"verifying"}, Phase: projectspec.GatePhaseExit},
	})
	sm := replayGateStateMachine()

	_, err := coord.ReplayGate(context.Background(), task, meta, sm, "nonexistent-gate")
	if err == nil {
		t.Fatal("expected error for nonexistent gate, got nil")
	}
}

func TestCoordinator_ReplayGate_GateNotMatchedForStatus(t *testing.T) {
	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: newMockExecutorWaiter(),
		GateExecutor: newMockExecutorWaiter(),
		Waiter:       newMockExecutorWaiter(),
	}

	task := &orchestrator.Task{
		ID:        "task-replay-5",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting, // gate expects "verifying"
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "verify-gate", On: orchestrator.OnValues{"verifying"}, Phase: projectspec.GatePhaseExit},
	})
	sm := replayGateStateMachine()

	_, err := coord.ReplayGate(context.Background(), task, meta, sm, "verify-gate")
	if err == nil {
		t.Fatal("expected error for gate not matching current status, got nil")
	}
}

func TestListGatesForStatus_ReturnsMatchingGates(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-1",
		Behavior: "dev",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "entry-gate", On: orchestrator.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry},
		{ID: "exit-gate", On: orchestrator.OnValues{"executing"}, Phase: projectspec.GatePhaseExit},
		{ID: "verify-gate", On: orchestrator.OnValues{"verifying"}, Phase: projectspec.GatePhaseExit},
	})

	gates := orchestrator.ListGatesForStatus(meta, task, orchestrator.TaskStatusExecuting)
	if len(gates) != 2 {
		t.Fatalf("expected 2 gates for executing, got %d", len(gates))
	}
	ids := map[string]bool{}
	for _, g := range gates {
		ids[g.ID] = true
	}
	if !ids["entry-gate"] || !ids["exit-gate"] {
		t.Errorf("expected entry-gate and exit-gate, got %v", ids)
	}
	if ids["verify-gate"] {
		t.Error("verify-gate must not appear for executing status")
	}
}

func TestListGatesForStatus_NoBehavior(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-list-2",
		Behavior: "unknown",
		Status:   orchestrator.TaskStatusExecuting,
		Payload:  json.RawMessage(`{}`),
	}
	meta := metaWithBehavior(nil, nil)

	gates := orchestrator.ListGatesForStatus(meta, task, orchestrator.TaskStatusExecuting)
	if len(gates) != 0 {
		t.Errorf("expected 0 gates for unknown behavior, got %d", len(gates))
	}
}
