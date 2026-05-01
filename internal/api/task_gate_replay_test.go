package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// replayGateCoordinator is a test double that records ReplayGate calls.
type replayGateCoordinator struct {
	replayResult *orchestrator.ReplayResult
	replayErr    error
	replayCalls  int
}

func (c *replayGateCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return &orchestrator.DispatchResult{FinalPayload: task.Payload}, nil
}

func (c *replayGateCoordinator) DispatchEntryGates(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta) (*orchestrator.EntryGateResult, error) {
	return &orchestrator.EntryGateResult{FinalPayload: task.Payload}, nil
}

func (c *replayGateCoordinator) ReplayGate(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, gateID string) (*orchestrator.ReplayResult, error) {
	c.replayCalls++
	if c.replayErr != nil {
		return nil, c.replayErr
	}
	if c.replayResult != nil {
		return c.replayResult, nil
	}
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

// TestTaskWorkflowService_ReplayGate_Basic verifies basic gate replay with status persisted.
func TestTaskWorkflowService_ReplayGate_Basic(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-gate-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}

	newPayload := json.RawMessage(`{"passed":true}`)
	coord := &replayGateCoordinator{
		replayResult: &orchestrator.ReplayResult{
			FinalPayload: newPayload,
			NewStatus:    orchestrator.TaskStatusDone,
		},
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task},
		Jobs:        &stubJobStore{},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Coordinator: coord,
		Tx:          recordingTransactor{store: txStore},
	}

	result, err := svc.ReplayGate(context.Background(), task.ID, ReplayGateRequest{GateID: "check-gate"})
	if err != nil {
		t.Fatalf("ReplayGate() error = %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if coord.replayCalls != 1 {
		t.Fatalf("ReplayGate called %d times, want 1", coord.replayCalls)
	}
	// Tx must have been used to persist the payload/status.
	if txStore.updatedTask == nil {
		t.Fatal("UpdateTask not called within transaction")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Errorf("persisted status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusDone)
	}
}

// TestTaskWorkflowService_ReplayGate_RunningJobConflict verifies 409 when a job is running.
func TestTaskWorkflowService_ReplayGate_RunningJobConflict(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-gate-3",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}
	runningJob := &Job{
		ID:     "job-running-1",
		TaskID: task.ID,
		Status: JobStatusRunning,
	}

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Jobs: &stubJobStore{
			jobsByTask: map[string][]*Job{task.ID: {runningJob}},
		},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	_, err := svc.ReplayGate(context.Background(), task.ID, ReplayGateRequest{GateID: "check-gate"})
	if err == nil {
		t.Fatal("expected 409 error for running job, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected StatusConflict, got %v", err)
	}
}

// TestTaskWorkflowService_ReplayGate_TaskNotFound verifies 404 for unknown task.
func TestTaskWorkflowService_ReplayGate_TaskNotFound(t *testing.T) {
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{},
		Jobs:  &stubJobStore{},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}},
	}

	_, err := svc.ReplayGate(context.Background(), "nonexistent", ReplayGateRequest{GateID: "check-gate"})
	if err == nil {
		t.Fatal("expected 404 error for nonexistent task, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}
