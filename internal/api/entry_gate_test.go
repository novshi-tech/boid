package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// entryGateCoordinator is a test double that allows controlling both
// DispatchAndAdvance and DispatchEntryGates results independently.
type entryGateCoordinator struct {
	advanceResults []*orchestrator.DispatchResult
	advanceIdx     int
	entryResult    *orchestrator.EntryGateResult
	entryCalls     int
}

func (c *entryGateCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	if c.advanceIdx < len(c.advanceResults) {
		r := c.advanceResults[c.advanceIdx]
		c.advanceIdx++
		return r, nil
	}
	return &orchestrator.DispatchResult{FinalPayload: task.Payload}, nil
}

func (c *entryGateCoordinator) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	c.entryCalls++
	if c.entryResult != nil {
		return c.entryResult, nil
	}
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, nil
}

func (c *entryGateCoordinator) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

func (c *entryGateCoordinator) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

func TestRunDispatchLoop_EntryGateFiresAfterAdvance(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	txStore := &recordingTxStore{task: task}
	coord := &entryGateCoordinator{
		advanceResults: []*orchestrator.DispatchResult{
			{
				FinalPayload: json.RawMessage(`{"artifact":"url"}`),
				NewStatus:    orchestrator.TaskStatusDone,
			},
		},
		entryResult: &orchestrator.EntryGateResult{
			FinalPayload: json.RawMessage(`{"artifact":"url","extra":"from-entry-gate"}`),
		},
	}

	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	// Entry gates should have fired for the executing → done transition
	if coord.entryCalls == 0 {
		t.Fatal("expected entry gates to fire after advance")
	}
}

func TestRunDispatchLoop_EntryGateOnDone_TriggersOnce(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}

	txStore := &recordingTxStore{task: task}
	coord := &entryGateCoordinator{
		advanceResults: []*orchestrator.DispatchResult{
			{
				FinalPayload: json.RawMessage(`{}`),
				NewStatus:    orchestrator.TaskStatusDone,
			},
		},
		entryResult: &orchestrator.EntryGateResult{
			FinalPayload: json.RawMessage(`{"artifact":{"pr":{"merged":true}}}`),
		},
	}
	lifecycle := &stubLifecycle{}

	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
		Lifecycle:   lifecycle,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	if coord.entryCalls != 1 {
		t.Fatalf("expected entry gates to fire exactly once on done, got %d", coord.entryCalls)
	}
	if lifecycle.cleanupTaskID != task.ID {
		t.Fatalf("expected lifecycle cleanup for task %s, got %s", task.ID, lifecycle.cleanupTaskID)
	}
}
