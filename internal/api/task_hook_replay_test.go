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

// TestTaskWorkflowService_ReplayHook_PreservesMidHookRPCWrite is the
// ReplayHook counterpart of
// TestTaskWorkflowServiceRunDispatchLoop_PreservesMidHookRPCWrite_ReopenScenario
// (Phase 5b PR7 codex review Blocker 1, wiring-seams.md #17). This callsite
// had an even blunter version of the same bug: it wholesale-assigned
// `latest.Payload = replay.FinalPayload` instead of merging anything, so ANY
// concurrent write (not just a stale one) was guaranteed to be discarded.
// Simulates the replayed hook's own job calling
// `boid task update --payload-patch` mid-flight (writes immediately to the
// DB) and then producing no file-based output of its own.
func TestTaskWorkflowService_ReplayHook_PreservesMidHookRPCWrite(t *testing.T) {
	staleReport := json.RawMessage(`{"artifact":{"report":{"summary":"OLD"}}}`)
	freshReport := json.RawMessage(`{"artifact":{"report":{"summary":"NEW via --payload-patch mid-replay"}}}`)

	// task is the snapshot ReplayHook fetched and handed to the coordinator
	// BEFORE the replayed hook's job ran.
	task := &orchestrator.Task{
		ID:        "task-hook-4",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   staleReport,
	}
	coord := &replayHookCoordinator{
		replayResult: &orchestrator.ReplayResult{
			// The hook produced no file-based output, so its own
			// PayloadPatch was empty and FinalPayload is just the stale
			// snapshot, unchanged; PayloadDelta correctly reflects "wrote
			// nothing".
			FinalPayload: staleReport,
			PayloadDelta: json.RawMessage(`{}`),
		},
	}
	taskStore := &stubTaskStore{task: task}
	// txStore's task represents the DB-fresh row by the time the persist
	// step re-reads it — already reflecting the mid-hook RPC write.
	dbTask := &orchestrator.Task{
		ID:        task.ID,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  task.Behavior,
		Payload:   freshReport,
	}
	txStore := &recordingTxStore{task: dbTask}
	svc := &TaskWorkflowService{
		Tasks:       taskStore,
		Jobs:        &stubJobStore{},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Coordinator: coord,
		Tx:          recordingTransactor{store: txStore},
	}

	_, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"})
	if err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}
	if txStore.updatedTask == nil {
		t.Fatal("UpdateTask not called within transaction")
	}
	var payload struct {
		Artifact struct {
			Report struct {
				Summary string `json:"summary"`
			} `json:"report"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(txStore.updatedTask.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload.Artifact.Report.Summary != "NEW via --payload-patch mid-replay" {
		t.Fatalf("report.summary = %q, want the mid-hook RPC write to survive — got the stale value, the exact bug this test guards against",
			payload.Artifact.Report.Summary)
	}
}
