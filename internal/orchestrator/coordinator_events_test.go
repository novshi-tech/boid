package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_DispatchAndAdvance_FiredEvents_HookKitID(t *testing.T) {
	mock := newMockExecutorWaiter()

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
		{ID: "go-dev/pr-verify", On: orchestrator.OnValues{"executing"}, Kit: "go-dev"},
	}, nil)
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.FiredEvents) != 1 {
		t.Fatalf("FiredEvents len = %d, want 1", len(result.FiredEvents))
	}
	fe := result.FiredEvents[0]
	if fe.KitID != "go-dev" {
		t.Errorf("KitID = %q, want %q", fe.KitID, "go-dev")
	}
	if fe.HandlerID != "go-dev/pr-verify" {
		t.Errorf("HandlerID = %q, want %q", fe.HandlerID, "go-dev/pr-verify")
	}
	if fe.Kind != "hook" {
		t.Errorf("Kind = %q, want %q", fe.Kind, "hook")
	}
	if fe.SourceState != "executing" {
		t.Errorf("SourceState = %q, want %q", fe.SourceState, "executing")
	}
	if !fe.Success {
		t.Errorf("Success = false, want true")
	}
}

func TestCoordinator_DispatchAndAdvance_FiredEvents_ExitGateKitID(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("go-dev/auto-merge", `{"payload_patch":{}}`, 0)

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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "go-dev/auto-merge", On: orchestrator.OnValues{"executing"}, Phase: projectspec.GatePhaseExit, Kit: "go-dev"},
	})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(result.FiredEvents) != 1 {
		t.Fatalf("FiredEvents len = %d, want 1", len(result.FiredEvents))
	}
	fe := result.FiredEvents[0]
	if fe.KitID != "go-dev" {
		t.Errorf("KitID = %q, want %q", fe.KitID, "go-dev")
	}
	if fe.Kind != "exit_gate" {
		t.Errorf("Kind = %q, want %q", fe.Kind, "exit_gate")
	}
	if !fe.Success {
		t.Errorf("Success = false, want true")
	}
}

func TestCoordinator_DispatchEntryGates_FiredEvents_EntryGateKitID(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("go-dev/fetch-jira", `{"payload_patch":{}}`, 0)

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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "go-dev/fetch-jira", On: orchestrator.OnValues{"executing"}, Phase: projectspec.GatePhaseEntry, Kit: "go-dev"},
	})

	entryResult, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("DispatchEntryGates: %v", err)
	}

	if len(entryResult.FiredEvents) != 1 {
		t.Fatalf("FiredEvents len = %d, want 1", len(entryResult.FiredEvents))
	}
	fe := entryResult.FiredEvents[0]
	if fe.KitID != "go-dev" {
		t.Errorf("KitID = %q, want %q", fe.KitID, "go-dev")
	}
	if fe.Kind != "entry_gate" {
		t.Errorf("Kind = %q, want %q", fe.Kind, "entry_gate")
	}
	if !fe.Success {
		t.Errorf("Success = false, want true")
	}
}
