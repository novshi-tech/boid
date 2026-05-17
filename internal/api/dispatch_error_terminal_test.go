package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestRunDispatchLoop_DispatchError_TerminatesTask verifies that a dispatch
// error transitions the task to aborted and records a dispatch_error action
// with aborted_reason="dispatch_error".
func TestRunDispatchLoop_DispatchError_TerminatesTask(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-de-term-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchErr: errors.New("hook dispatch: hook failed: exit code 1"),
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	// Task must be transitioned to aborted.
	if txStore.updatedTask == nil {
		t.Fatal("UpdateTask not called; task should be transitioned to aborted")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Errorf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusAborted)
	}

	// dispatch_error action must be recorded.
	var de *orchestrator.Action
	for _, a := range txStore.actions {
		if a.Type == "dispatch_error" {
			de = a
			break
		}
	}
	if de == nil {
		t.Fatal("dispatch_error action not found")
	}

	// ToStatus must be aborted.
	if de.ToStatus != orchestrator.TaskStatusAborted {
		t.Errorf("dispatch_error action ToStatus = %q, want %q", de.ToStatus, orchestrator.TaskStatusAborted)
	}

	// Payload must carry aborted_reason="dispatch_error".
	var payload map[string]any
	if err := json.Unmarshal(de.Payload, &payload); err != nil {
		t.Fatalf("unmarshal dispatch_error payload: %v", err)
	}
	if payload["aborted_reason"] != "dispatch_error" {
		t.Errorf("aborted_reason = %v, want %q", payload["aborted_reason"], "dispatch_error")
	}
}

// TestRunDispatchLoop_DispatchError_WithWorktreeSetupError_IncludesCause verifies
// that when the dispatch error wraps a *dispatcher.WorktreeSetupError, the cause
// field is propagated into the dispatch_error action payload.
func TestRunDispatchLoop_DispatchError_WithWorktreeSetupError_IncludesCause(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-de-cause-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}

	wse := &dispatcher.WorktreeSetupError{
		Cause: "missing_initial_commit",
		Hint:  "create an initial commit",
		Err:   errors.New(`project "/tmp/repo" has no commits yet`),
	}
	coord := &hookFiredCoordinator{
		dispatchErr: wse,
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	var de *orchestrator.Action
	for _, a := range txStore.actions {
		if a.Type == "dispatch_error" {
			de = a
			break
		}
	}
	if de == nil {
		t.Fatal("dispatch_error action not found")
	}

	var payload map[string]any
	if err := json.Unmarshal(de.Payload, &payload); err != nil {
		t.Fatalf("unmarshal dispatch_error payload: %v", err)
	}
	if payload["cause"] != "missing_initial_commit" {
		t.Errorf("cause = %v, want %q", payload["cause"], "missing_initial_commit")
	}
	if payload["aborted_reason"] != "dispatch_error" {
		t.Errorf("aborted_reason = %v, want %q", payload["aborted_reason"], "dispatch_error")
	}
}

// TestRunDispatchLoop_EntryGateError_TerminatesTask verifies that an entry gate
// dispatch failure also transitions the task to aborted.
func TestRunDispatchLoop_EntryGateError_TerminatesTask(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-de-eg-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchResult: &orchestrator.DispatchResult{
			FinalPayload: task.Payload,
			NewStatus:    orchestrator.TaskStatusDone,
		},
		entryErr: errors.New(`entry gate dispatch: gate "verify" failed: exit code 2`),
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
	}

	svc.runDispatchLoop(context.Background(), task, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	if txStore.updatedTask == nil {
		t.Fatal("UpdateTask not called; task should be transitioned to aborted")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Errorf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusAborted)
	}

	var de *orchestrator.Action
	for _, a := range txStore.actions {
		if a.Type == "dispatch_error" {
			de = a
			break
		}
	}
	if de == nil {
		t.Fatal("dispatch_error action not found")
	}
	if de.ToStatus != orchestrator.TaskStatusAborted {
		t.Errorf("dispatch_error action ToStatus = %q, want %q", de.ToStatus, orchestrator.TaskStatusAborted)
	}
}

// TestRunDispatchLoop_DispatchError_ReleasesLock verifies that a dispatch error
// releases the project lock (Locks.IsHeldForTask returns false after the error).
func TestRunDispatchLoop_DispatchError_ReleasesLock(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-de-lock-1",
		ProjectID: "proj-lock-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}

	coord := &hookFiredCoordinator{
		dispatchErr: errors.New("hook dispatch: hook failed: exit code 1"),
	}
	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())

	// Pre-acquire the lock as if the dispatch loop acquired it.
	if err := locks.AcquireForTask(context.Background(), task.ProjectID, task.ID); err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}
	if !locks.IsHeldForTask(task.ID) {
		t.Fatal("lock should be held before dispatch error")
	}

	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
		Locks:       locks,
	}

	svc.terminateForDispatchError(context.Background(), task, errors.New("hook failed"))

	if locks.IsHeldForTask(task.ID) {
		t.Error("lock should be released after terminateForDispatchError")
	}
}

// TestRunDispatchLoop_DispatchError_LockReleasedAllowsNextTask verifies that
// after a dispatch error the project lock is released, which lets a second
// pending task acquire the lock and start.
func TestRunDispatchLoop_DispatchError_LockReleasedAllowsNextTask(t *testing.T) {
	task1 := &orchestrator.Task{
		ID:        "task-de-next-1",
		ProjectID: "proj-next-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}
	task2 := &orchestrator.Task{
		ID:        "task-de-next-2",
		ProjectID: "proj-next-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "executor",
		Payload:   json.RawMessage(`{}`),
	}

	locks := orchestrator.NewProjectLockManager(orchestrator.NewInMemoryWorktreeLockManager())
	// Pre-acquire the lock on behalf of task1.
	if err := locks.AcquireForTask(context.Background(), task1.ProjectID, task1.ID); err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}

	coord := &hookFiredCoordinator{
		dispatchErr: errors.New("hook dispatch failed"),
	}
	txStore := &recordingTxStore{task: task1}
	svc := &TaskWorkflowService{
		Tx:          recordingTransactor{store: txStore},
		Coordinator: coord,
		Locks:       locks,
	}

	svc.runDispatchLoop(context.Background(), task1, &orchestrator.ProjectMeta{}, orchestrator.DefaultMachine())

	// After dispatch_error, task1's lock must be released.
	if locks.IsHeldForTask(task1.ID) {
		t.Error("task1 lock should be released after dispatch error")
	}

	// task2 can now acquire the project lock (it was blocked by task1's lock before).
	acquireCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := locks.AcquireForTask(acquireCtx, task2.ProjectID, task2.ID); err != nil {
		t.Errorf("task2 should be able to acquire the lock after task1's dispatch error, got: %v", err)
	}
}
