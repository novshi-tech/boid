package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// replayHookCoordinator is a test double that records ReplayHook calls.
type replayHookCoordinator struct {
	replayResult *orchestrator.ReplayResult
	replayErr    error
	replayCalls  int
}

func (c *replayHookCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return &orchestrator.DispatchResult{FinalPayload: task.Payload}, nil
}

func (c *replayHookCoordinator) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	c.replayCalls++
	if c.replayErr != nil {
		return nil, c.replayErr
	}
	if c.replayResult != nil {
		return c.replayResult, nil
	}
	return &orchestrator.ReplayResult{FinalPayload: task.Payload}, nil
}

// TestTaskWorkflowService_ReplayHook_Basic verifies basic hook replay with status persisted.
func TestTaskWorkflowService_ReplayHook_Basic(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hook-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}

	newPayload := json.RawMessage(`{"step":"done"}`)
	coord := &replayHookCoordinator{
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

	result, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"})
	if err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if coord.replayCalls != 1 {
		t.Fatalf("ReplayHook called %d times, want 1", coord.replayCalls)
	}
	// Tx must have been used to persist the payload/status.
	if txStore.updatedTask == nil {
		t.Fatal("UpdateTask not called within transaction")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Errorf("persisted status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusDone)
	}
}

// TestTaskWorkflowService_ReplayHook_RunningJobConflict verifies 409 when a job is running.
func TestTaskWorkflowService_ReplayHook_RunningJobConflict(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hook-2",
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

	_, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"})
	if err == nil {
		t.Fatal("expected 409 error for running job, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected StatusConflict, got %v", err)
	}
}

// TestTaskWorkflowService_ReplayHook_TaskNotFound verifies 404 for unknown task.
func TestTaskWorkflowService_ReplayHook_TaskNotFound(t *testing.T) {
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{},
		Jobs:  &stubJobStore{},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}},
	}

	_, err := svc.ReplayHook(context.Background(), "nonexistent", ReplayHookRequest{HookID: "main-hook"})
	if err == nil {
		t.Fatal("expected 404 error for nonexistent task, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

// TestTaskWorkflowService_ReplayHook_StatusOverride verifies status override before replay.
func TestTaskWorkflowService_ReplayHook_StatusOverride(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hook-3",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{}`),
	}

	coord := &replayHookCoordinator{}
	taskStore := &stubTaskStore{task: task}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:       taskStore,
		Jobs:        &stubJobStore{},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Coordinator: coord,
		Tx:          recordingTransactor{store: txStore},
	}

	_, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{
		HookID: "main-hook",
		Status: "executing",
	})
	if err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}
	// Status override must have been applied before dispatch.
	if taskStore.updateCalls == 0 {
		t.Error("UpdateTask not called for status override")
	}
}
