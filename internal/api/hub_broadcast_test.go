package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// alwaysFailTx simulates a Transactor whose WithinTx always returns an error,
// modeling a commit failure without executing the provided function.
type alwaysFailTx struct{}

func (t alwaysFailTx) WithinTx(fn func(TxStore) error) error {
	return fmt.Errorf("simulated commit failure")
}

func receiveEvent(t *testing.T, ch <-chan TaskEvent, timeout time.Duration) (TaskEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		return TaskEvent{}, false
	}
}

// TestApplyAction_BroadcastsActionOnCommitSuccess verifies that ApplyAction
// broadcasts a "action" event to the hub after a successful commit.
func TestApplyAction_BroadcastsActionOnCommitSuccess(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hub-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), task.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Hub:   hub,
	}

	result, err := svc.ApplyAction(t.Context(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want executing", result.Task.Status)
	}

	ev, ok := receiveEvent(t, ch, time.Second)
	if !ok {
		t.Fatal("hub did not receive broadcast event after ApplyAction success")
	}
	if ev.Kind != "action" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "action")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("event payload type = %T, want map[string]any", ev.Payload)
	}
	if payload["new_status"] != string(orchestrator.TaskStatusExecuting) {
		t.Fatalf("new_status = %v, want %q", payload["new_status"], orchestrator.TaskStatusExecuting)
	}
}

// TestApplyAction_NoBroadcastOnCommitFailure verifies that no event is broadcast
// when the transaction commit fails in ApplyAction.
func TestApplyAction_NoBroadcastOnCommitFailure(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hub-fail",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), task.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    alwaysFailTx{},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Hub:   hub,
	}

	_, err := svc.ApplyAction(t.Context(), task.ID, ApplyActionRequest{Type: "start"})
	if err == nil {
		t.Fatal("ApplyAction() expected error on commit failure, got nil")
	}

	_, ok := receiveEvent(t, ch, 50*time.Millisecond)
	if ok {
		t.Fatal("hub must not receive broadcast event when commit fails")
	}
}

// TestCompleteJob_BroadcastsJobOnJobFailedCommitSuccess verifies that CompleteJob
// broadcasts a "job" event when a failed job (exit != 0) triggers the job_failed
// transition and the commit succeeds.
func TestCompleteJob_BroadcastsJobOnJobFailedCommitSuccess(t *testing.T) {
	job := &Job{
		ID:        "job-hub-1",
		TaskID:    "task-hub-2",
		ProjectID: "proj-1",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-hub-2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}

	txStore := &stubTx{}
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), task.ID)

	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{task: task},
		Jobs:      &stubJobStore{job: job},
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: &stubLifecycle{},
		Tx:        txStore,
		Hub:       hub,
	}

	_, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 1})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}

	ev, ok := receiveEvent(t, ch, time.Second)
	if !ok {
		t.Fatal("hub did not receive broadcast event after CompleteJob job_failed commit success")
	}
	if ev.Kind != "job" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "job")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("event payload type = %T, want map[string]any", ev.Payload)
	}
	if payload["job_id"] != job.ID {
		t.Fatalf("job_id = %v, want %q", payload["job_id"], job.ID)
	}
	if payload["new_state"] != string(orchestrator.TaskStatusAborted) {
		t.Fatalf("new_state = %v, want %q", payload["new_state"], orchestrator.TaskStatusAborted)
	}
}

// TestCompleteJob_NoBroadcastOnCommitFailure verifies that CompleteJob does not
// broadcast when the job_failed transaction commit fails.
func TestCompleteJob_NoBroadcastOnCommitFailure(t *testing.T) {
	job := &Job{
		ID:        "job-hub-fail",
		TaskID:    "task-hub-fail2",
		ProjectID: "proj-1",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-hub-fail2",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), task.ID)

	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{task: task},
		Jobs:      &stubJobStore{job: job},
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: &stubLifecycle{},
		Tx:        alwaysFailTx{},
		Hub:       hub,
	}

	_, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 1})
	if err == nil {
		t.Fatal("CompleteJob() expected error on commit failure, got nil")
	}

	_, ok := receiveEvent(t, ch, 50*time.Millisecond)
	if ok {
		t.Fatal("hub must not receive broadcast event when commit fails")
	}
}

// TestPersistFiredEvents_BroadcastsOnSuccess verifies that persistFiredEvents
// broadcasts one "fired_event" per FiredEvent after commit success.
func TestPersistFiredEvents_BroadcastsOnSuccess(t *testing.T) {
	txStore := &recordingTxStore{task: &orchestrator.Task{ID: "task-pe-1"}}
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), "task-pe-1")

	svc := &TaskWorkflowService{
		Tx:  recordingTransactor{store: txStore},
		Hub: hub,
	}

	events := []orchestrator.FiredEvent{
		{KitID: "kit-a", HandlerID: "hook-x", Kind: "hook", Success: true},
		{KitID: "kit-b", HandlerID: "exit-gate-y", Kind: "exit_gate", Success: false, Error: "fail"},
	}

	svc.persistFiredEvents("task-pe-1", orchestrator.TaskStatusExecuting, events)

	for i, fe := range events {
		ev, ok := receiveEvent(t, ch, time.Second)
		if !ok {
			t.Fatalf("event %d: hub did not receive broadcast after persistFiredEvents commit success", i)
		}
		if ev.Kind != "fired_event" {
			t.Fatalf("event %d: kind = %q, want %q", i, ev.Kind, "fired_event")
		}
		payload, ok := ev.Payload.(map[string]any)
		if !ok {
			t.Fatalf("event %d: payload type = %T, want map[string]any", i, ev.Payload)
		}
		wantEventName := fe.Kind + "_fired"
		if payload["event_name"] != wantEventName {
			t.Fatalf("event %d: event_name = %v, want %q", i, payload["event_name"], wantEventName)
		}
		if payload["role"] != fe.HandlerID {
			t.Fatalf("event %d: role = %v, want %q", i, payload["role"], fe.HandlerID)
		}
	}
}

// TestPersistFiredEvents_NoBroadcastOnCommitFailure verifies that persistFiredEvents
// does not broadcast when the transaction commit fails.
func TestPersistFiredEvents_NoBroadcastOnCommitFailure(t *testing.T) {
	hub := NewTaskEventHub()
	ch := hub.Subscribe(t.Context(), "task-pe-fail")

	svc := &TaskWorkflowService{
		Tx:  alwaysFailTx{},
		Hub: hub,
	}

	events := []orchestrator.FiredEvent{
		{KitID: "kit-a", HandlerID: "hook-x", Kind: "hook", Success: true},
	}

	svc.persistFiredEvents("task-pe-fail", orchestrator.TaskStatusExecuting, events)

	_, ok := receiveEvent(t, ch, 50*time.Millisecond)
	if ok {
		t.Fatal("hub must not receive broadcast event when commit fails")
	}
}
