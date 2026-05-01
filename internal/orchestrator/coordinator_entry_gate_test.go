package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCoordinator_DispatchEntryGates_NoMatch(t *testing.T) {
	mock := newMockExecutorWaiter()
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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "exit-gate", Phase: projectspec.GatePhaseExit},
	})

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(result.Results))
	}
	if string(result.FinalPayload) != string(task.Payload) {
		t.Fatalf("expected payload unchanged, got %s", result.FinalPayload)
	}
}

func TestCoordinator_DispatchEntryGates_SingleGate(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("fetch-jira", `{"payload_patch":{"artifact":{"jira":"PROJ-1"}}}`, 0)

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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{
			ID:    "fetch-jira",
			Phase: projectspec.GatePhaseEntry,
			Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	})

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := payload["artifact"]; !ok {
		t.Fatal("expected artifact in final payload")
	}
}

func TestCoordinator_DispatchEntryGates_EmptyOutput(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("noop-gate", `{"payload_patch":{}}`, 0)

	coord := &orchestrator.Coordinator{
		Evaluator:    &orchestrator.Evaluator{},
		HookExecutor: mock,
		GateExecutor: mock,
		Waiter:       mock,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{"artifact":"existing"}`),
	}
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{
			ID:    "noop-gate",
			Phase: projectspec.GatePhaseEntry,
			Traits: projectspec.HandlerTraits{
				Produces: []projectspec.TraitType{projectspec.TraitArtifact},
			},
		},
	})

	result, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty payload_patch should not alter existing payload
	if string(result.FinalPayload) != `{"artifact":"existing"}` {
		t.Fatalf("expected payload unchanged, got %s", result.FinalPayload)
	}
}

func TestCoordinator_DispatchEntryGates_ExclusiveCollision(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("gate-a", `{"payload_patch":{"artifact":"a"}}`, 0)
	mock.setGateCompletion("gate-b", `{"payload_patch":{"artifact":"b"}}`, 0)

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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "gate-a", Phase: projectspec.GatePhaseEntry,
			Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
		{ID: "gate-b", Phase: projectspec.GatePhaseEntry,
			Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
	})

	_, err := coord.DispatchEntryGates(context.Background(), task, meta)
	if err == nil {
		t.Fatal("expected exclusive trait collision error")
	}
}

func TestCoordinator_DispatchAndAdvance_IgnoresEntryGates(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setGateCompletion("entry-gate", `{"payload_patch":{"artifact":"should-not-appear"}}`, 0)

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
	meta := metaWithBehavior(nil, []projectspec.Gate{
		{ID: "entry-gate", Phase: projectspec.GatePhaseEntry,
			Traits: projectspec.HandlerTraits{Produces: []projectspec.TraitType{projectspec.TraitArtifact}}},
	})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var payload map[string]json.RawMessage
	json.Unmarshal(result.FinalPayload, &payload)
	if _, ok := payload["artifact"]; ok {
		t.Error("DispatchAndAdvance should not fire entry gates")
	}
}
