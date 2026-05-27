package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// When daemon shuts down via SIGTERM, the dispatch ctx is canceled and any
// hook in flight returns context.Canceled. abortOnDispatchError must record
// a `daemon_shutdown` abort code (not `dispatch_error`) so the start-time
// auto-reopen logic can find the task. dispatch_error actions must NOT be
// recorded for shutdown — those exist for genuine hook failures.
func TestAbortOnDispatchError_CtxCanceled_UsesDaemonShutdownCode(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-shut-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.abortOnDispatchError(ctx, task, context.Canceled)

	var abortAction *orchestrator.Action
	var dispatchErrCount int
	for _, a := range txStore.actions {
		if a.Type == "abort" {
			abortAction = a
		}
		if a.Type == "dispatch_error" {
			dispatchErrCount++
		}
	}

	if abortAction == nil {
		t.Fatal("expected abort action to be recorded")
	}
	var payload map[string]string
	if err := json.Unmarshal(abortAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal abort payload: %v", err)
	}
	if payload["code"] != "daemon_shutdown" {
		t.Errorf("expected abort code daemon_shutdown, got %s", payload["code"])
	}
	if dispatchErrCount != 0 {
		t.Errorf("expected no dispatch_error action when ctx canceled, got %d", dispatchErrCount)
	}
}

// When the dispatch context is live but the error wraps context.Canceled
// (e.g. a child hook ctx propagated cancellation), still treat it as a
// daemon shutdown so the reopen path picks it up.
func TestAbortOnDispatchError_WrappedCanceled_UsesDaemonShutdownCode(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-shut-2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
	}

	// ctx itself is live; the err carries the cancellation.
	wrapped := errors.Join(errors.New("hook dispatch:"), context.Canceled)
	svc.abortOnDispatchError(context.Background(), task, wrapped)

	var abortAction *orchestrator.Action
	for _, a := range txStore.actions {
		if a.Type == "abort" {
			abortAction = a
		}
	}
	if abortAction == nil {
		t.Fatal("expected abort action to be recorded")
	}
	var payload map[string]string
	if err := json.Unmarshal(abortAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal abort payload: %v", err)
	}
	if payload["code"] != "daemon_shutdown" {
		t.Errorf("expected abort code daemon_shutdown, got %s", payload["code"])
	}
}

// Regression: genuine hook failures (not shutdown) must still use the
// dispatch_error code path. This is the existing behaviour preserved.
func TestAbortOnDispatchError_NonCanceled_UsesDispatchErrorCode(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-disperr-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
	}

	svc.abortOnDispatchError(context.Background(), task, errors.New("hook failure"))

	var abortAction *orchestrator.Action
	var dispatchErrCount int
	for _, a := range txStore.actions {
		if a.Type == "abort" {
			abortAction = a
		}
		if a.Type == "dispatch_error" {
			dispatchErrCount++
		}
	}
	if abortAction == nil {
		t.Fatal("expected abort action to be recorded")
	}
	var payload map[string]string
	if err := json.Unmarshal(abortAction.Payload, &payload); err != nil {
		t.Fatalf("unmarshal abort payload: %v", err)
	}
	if payload["code"] != "dispatch_error" {
		t.Errorf("expected abort code dispatch_error, got %s", payload["code"])
	}
	if dispatchErrCount != 1 {
		t.Errorf("expected 1 dispatch_error action, got %d", dispatchErrCount)
	}
}
