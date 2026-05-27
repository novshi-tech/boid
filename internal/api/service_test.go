package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// newCmd is a tiny wrapper around exec.Command that exists so the Phase 2-2
// test helpers can stay consistent if we ever need to inject a shared env.
func newCmd(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

// TestCompleteJobSuccessNotifiesWithoutTransition verifies that a successful
// (exit 0) job completion only records the job result and notifies Lifecycle.
// No task state transition should occur: that is driven exclusively by
// DispatchAndAdvance (condition-based auto-advance).
func TestCompleteJobSuccessNotifiesWithoutTransition(t *testing.T) {
	job := &Job{
		ID:        "job-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}

	taskStore := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tasks:     taskStore,
		Jobs:      jobs,
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: lifecycle,
	}

	got, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 0, Output: "ok"})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	if got.Status != JobStatusCompleted {
		t.Fatalf("job status = %q, want %q", got.Status, JobStatusCompleted)
	}
	if jobs.updateCalls != 1 {
		t.Fatalf("UpdateJob calls = %d, want 1", jobs.updateCalls)
	}
	// Task state must NOT be updated by CompleteJob for a successful job.
	if taskStore.updateCalls != 0 {
		t.Fatalf("UpdateTask calls = %d, want 0 (no state transition on job_completed)", taskStore.updateCalls)
	}
	if lifecycle.completedJobID != job.ID {
		t.Fatalf("CompleteJob notified %q, want %q", lifecycle.completedJobID, job.ID)
	}
	if lifecycle.unregisteredJobID != job.ID {
		t.Fatalf("UnregisterJob called with %q, want %q", lifecycle.unregisteredJobID, job.ID)
	}
	if lifecycle.result.ExitCode != 0 || lifecycle.result.Output != "ok" {
		t.Fatalf("completion result = %+v, want exit 0 output ok", lifecycle.result)
	}
}

// TestCompleteJobFailureTransitionsToAborted verifies that a failed (exit != 0)
// job completion applies the job_failed action and transitions the task to aborted.
func TestCompleteJobFailureTransitionsToAborted(t *testing.T) {
	job := &Job{
		ID:        "job-3",
		TaskID:    "task-3",
		ProjectID: "proj-3",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-3",
		ProjectID: "proj-3",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}

	taskStore := &stubTaskStore{task: task}
	jobs := &stubJobStore{job: job}
	lifecycle := &stubLifecycle{}
	tx := &stubTx{}
	svc := &TaskWorkflowService{
		Tasks:     taskStore,
		Jobs:      jobs,
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: lifecycle,
		Tx:        tx,
	}

	got, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 1, Output: "boom"})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	if got.Status != JobStatusFailed {
		t.Fatalf("job status = %q, want %q", got.Status, JobStatusFailed)
	}
	if tx.updatedTask == nil {
		t.Fatal("UpdateTask not called, want aborted transition")
	}
	if tx.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("task status = %q, want %q", tx.updatedTask.Status, orchestrator.TaskStatusAborted)
	}
	if lifecycle.completedJobID != job.ID {
		t.Fatalf("CompleteJob notified %q, want %q", lifecycle.completedJobID, job.ID)
	}
}

// TestApplyAction_RecordsFromToStatus verifies that ApplyAction records the
// correct from/to status transition in the created Action.
func TestApplyAction_RecordsFromToStatus(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Title:     "test task",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "impl",
		Payload:   []byte(`{}`),
	}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
	}

	result, err := svc.ApplyAction(t.Context(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}
	if len(txStore.actions) != 1 {
		t.Fatalf("created actions = %d, want 1", len(txStore.actions))
	}
	action := txStore.actions[0]
	if action.FromStatus != orchestrator.TaskStatusPending {
		t.Fatalf("action.FromStatus = %q, want %q", action.FromStatus, orchestrator.TaskStatusPending)
	}
	if action.ToStatus != orchestrator.TaskStatusExecuting {
		t.Fatalf("action.ToStatus = %q, want %q", action.ToStatus, orchestrator.TaskStatusExecuting)
	}
}

// TestCompleteJob_JobFailed_RecordsFromToStatus verifies that a failed job
// records the correct from/to status in the created Action.
func TestCompleteJob_JobFailed_RecordsFromToStatus(t *testing.T) {
	job := &Job{
		ID:        "job-10",
		TaskID:    "task-10",
		ProjectID: "proj-10",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-10",
		ProjectID: "proj-10",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}

	tx := &stubTx{}
	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{task: task},
		Jobs:      &stubJobStore{job: job},
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: &stubLifecycle{},
		Tx:        tx,
	}

	_, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 1})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	if tx.createdAction == nil {
		t.Fatal("expected action to be recorded")
	}
	if tx.createdAction.FromStatus != orchestrator.TaskStatusExecuting {
		t.Fatalf("action.FromStatus = %q, want %q", tx.createdAction.FromStatus, orchestrator.TaskStatusExecuting)
	}
	if tx.createdAction.ToStatus != orchestrator.TaskStatusAborted {
		t.Fatalf("action.ToStatus = %q, want %q", tx.createdAction.ToStatus, orchestrator.TaskStatusAborted)
	}
}

// TestCompleteJobIdempotency verifies that a second call to CompleteJob for a
// terminal job returns the existing job without re-firing lifecycle events.
func TestCompleteJobIdempotency(t *testing.T) {
	job := &Job{
		ID:        "job-idem",
		TaskID:    "task-idem",
		ProjectID: "proj-idem",
		Status:    JobStatusCompleted, // already terminal
		ExitCode:  0,
	}
	lifecycle := &stubLifecycle{}
	jobs := &stubJobStore{job: job}
	svc := &TaskWorkflowService{
		Jobs:      jobs,
		Lifecycle: lifecycle,
	}

	got, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 0, Output: "second call"})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	if got.Status != JobStatusCompleted {
		t.Fatalf("job status = %q, want %q", got.Status, JobStatusCompleted)
	}
	// DB must not be touched again.
	if jobs.updateCalls != 0 {
		t.Fatalf("UpdateJob calls = %d, want 0 (idempotent)", jobs.updateCalls)
	}
	// Lifecycle must not be re-fired.
	if lifecycle.completedJobID != "" {
		t.Fatalf("CompleteJob lifecycle called with %q, want empty (no re-fire)", lifecycle.completedJobID)
	}
}

// TestCompleteJobStopsRuntime verifies that CompleteJob sends a StopJobRuntime
// signal when the job has a RuntimeID set.
func TestCompleteJobStopsRuntime(t *testing.T) {
	job := &Job{
		ID:        "job-rt",
		TaskID:    "task-rt",
		ProjectID: "proj-rt",
		Status:    JobStatusRunning,
		RuntimeID: "runtime-abc",
	}
	lifecycle := &stubLifecycle{}
	jobs := &stubJobStore{job: job}
	svc := &TaskWorkflowService{
		Jobs:      jobs,
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: lifecycle,
	}

	_, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 0})
	if err != nil {
		t.Fatalf("CompleteJob() error = %v", err)
	}
	// Give the goroutine a moment to run.
	deadline := 100 * time.Millisecond
	start := time.Now()
	for lifecycle.StoppedRuntimeID() == "" && time.Since(start) < deadline {
		time.Sleep(time.Millisecond)
	}
	if got := lifecycle.StoppedRuntimeID(); got != job.RuntimeID {
		t.Fatalf("StopJobRuntime called with %q, want %q", got, job.RuntimeID)
	}
}

func TestTaskAppServiceCreateTask_BehaviorNotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test task",
		Behavior:  "unknown-behavior",
	})
	if err == nil {
		t.Fatal("CreateTask() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("error code = %d, want %d", se.Code, http.StatusBadRequest)
	}
	want := `behavior "unknown-behavior" not found`
	if se.Message != want {
		t.Fatalf("error message = %q, want %q", se.Message, want)
	}
}

func TestTaskAppServiceCreateTask_ProjectNotInMeta_Skips(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil}, // Get returns false → skip validation
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-unknown",
		Title:     "test task",
		Behavior:  "any-behavior",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil", err)
	}
	if task == nil {
		t.Fatal("CreateTask() task = nil, want task")
	}
}

func TestTaskAppServiceGetTaskDetail(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Title:     "Implement observability",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
		Payload:   json.RawMessage(`{"artifact":{"url":"https://example.com"}}`),
	}
	actions := []*orchestrator.Action{{
		ID:      "action-1",
		TaskID:  task.ID,
		Type:    "start",
		Payload: json.RawMessage(`{"source":"cli"}`),
	}}
	jobs := []*Job{{
		ID:        "job-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		HandlerID: "build-artifact",
		Role:      "hook",
		Status:    JobStatusRunning,
	}}

	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: stubActionStore{actions: actions},
		Jobs:    &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}
	if got.Task.ID != task.ID {
		t.Fatalf("task id = %q, want %q", got.Task.ID, task.ID)
	}
	if len(got.Actions) != 1 || got.Actions[0].ID != "action-1" {
		t.Fatalf("actions = %+v, want action-1", got.Actions)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].ID != "job-1" {
		t.Fatalf("jobs = %+v, want job-1", got.Jobs)
	}
}

func TestTaskAppServiceGetTaskDetail_AvailableActions(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-aa",
		ProjectID: "proj-aa",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
	}
	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}
	hasStart, hasAbort := false, false
	for _, a := range got.AvailableActions {
		switch a {
		case "start":
			hasStart = true
		case "abort":
			hasAbort = true
		case "job_failed":
			t.Errorf("job_failed must not appear in AvailableActions")
		}
	}
	if !hasStart {
		t.Errorf("AvailableActions should contain 'start' for pending task, got %v", got.AvailableActions)
	}
	if !hasAbort {
		t.Errorf("AvailableActions should contain 'abort' for pending task, got %v", got.AvailableActions)
	}
}

func TestTaskWorkflowServiceCompleteJobFailedProjectMetaMissing(t *testing.T) {
	job := &Job{
		ID:        "job-2",
		TaskID:    "task-2",
		ProjectID: "proj-2",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-2",
		ProjectID: "proj-2",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "impl",
	}

	jobs := &stubJobStore{job: job}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{task: task},
		Jobs:      jobs,
		Meta:      stubMetaStore{meta: nil}, // meta not loaded → error on failed job
		Lifecycle: lifecycle,
	}

	_, err := svc.CompleteJob(t.Context(), job.ID, JobDoneRequest{ExitCode: 1, Output: "boom"})
	if err == nil {
		t.Fatal("CompleteJob() error = nil, want error")
	}
	if jobs.updateCalls != 1 {
		t.Fatalf("UpdateJob calls = %d, want 1", jobs.updateCalls)
	}
	if jobs.job.Status != JobStatusFailed {
		t.Fatalf("job status = %q, want %q", jobs.job.Status, JobStatusFailed)
	}
	if lifecycle.completedJobID != job.ID {
		t.Fatalf("CompleteJob notified %q, want %q", lifecycle.completedJobID, job.ID)
	}
	if lifecycle.unregisteredJobID != job.ID {
		t.Fatalf("UnregisterJob called with %q, want %q", lifecycle.unregisteredJobID, job.ID)
	}
	if lifecycle.result.ExitCode != 1 || lifecycle.result.Output != "boom" {
		t.Fatalf("completion result = %+v, want exit 1 output boom", lifecycle.result)
	}
}

func TestTaskAppServiceDeleteTask(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-1",
		Status:   orchestrator.TaskStatusDone,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	if err := svc.DeleteTask("task-1", false); err != nil {
		t.Fatalf("DeleteTask() error = %v", err)
	}
	if !store.deleted {
		t.Fatal("expected store.DeleteTask to be called")
	}
}

func TestTaskAppServiceDeleteTask_ActiveStatusBlockedWithoutForce(t *testing.T) {
	activeStatuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
	}
	for _, status := range activeStatuses {
		task := &orchestrator.Task{
			ID:       "task-1",
			Status:   status,
			Behavior: "dev",
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		err := svc.DeleteTask("task-1", false)
		if err == nil {
			t.Fatalf("status %s: expected error without --force", status)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusConflict {
			t.Fatalf("status %s: expected StatusConflict, got %v", status, err)
		}
		if store.deleted {
			t.Fatalf("status %s: store.DeleteTask should not be called", status)
		}
	}
}

func TestTaskAppServiceDeleteTask_ActiveStatusAllowedWithForce(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-1",
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	if err := svc.DeleteTask("task-1", true); err != nil {
		t.Fatalf("DeleteTask with force error = %v", err)
	}
	if !store.deleted {
		t.Fatal("expected store.DeleteTask to be called with force")
	}
}

func TestTaskAppServiceDeleteTask_NotFound(t *testing.T) {
	store := &stubTaskStore{err: fmt.Errorf("task not found")}
	svc := &TaskAppService{Tasks: store}

	err := svc.DeleteTask("nonexistent", false)
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func TestTaskAppServiceUpdateTask(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:          "task-1",
			Title:       "old title",
			Description: "old desc",
			Status:      orchestrator.TaskStatusPending,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		got, err := svc.UpdateTask("task-1", UpdateTaskRequest{Title: "new title", Description: "new desc"})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		if got.Title != "new title" {
			t.Fatalf("Title = %q, want %q", got.Title, "new title")
		}
		if got.Description != "new desc" {
			t.Fatalf("Description = %q, want %q", got.Description, "new desc")
		}
		if store.updateCalls != 1 {
			t.Fatalf("UpdateTask calls = %d, want 1", store.updateCalls)
		}
	})

	t.Run("not found", func(t *testing.T) {
		store := &stubTaskStore{err: fmt.Errorf("task not found")}
		svc := &TaskAppService{Tasks: store}

		_, err := svc.UpdateTask("nonexistent", UpdateTaskRequest{Title: "x"})
		if err == nil {
			t.Fatal("UpdateTask() error = nil, want error")
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusNotFound {
			t.Fatalf("expected StatusNotFound, got %v", err)
		}
	})

	t.Run("title update rejected when not pending", func(t *testing.T) {
		for _, status := range []orchestrator.TaskStatus{
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusAwaiting,
			orchestrator.TaskStatusDone,
			orchestrator.TaskStatusAborted,
		} {
			task := &orchestrator.Task{ID: "task-x", Title: "old", Status: status}
			store := &stubTaskStore{task: task}
			svc := &TaskAppService{Tasks: store}

			_, err := svc.UpdateTask("task-x", UpdateTaskRequest{Title: "new"})
			if err == nil {
				t.Fatalf("status=%s: expected conflict error, got nil", status)
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("status=%s: expected StatusConflict, got %v", status, err)
			}
		}
	})

	t.Run("project update success", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:        "task-p",
			Title:     "t",
			ProjectID: "proj-old",
			Status:    orchestrator.TaskStatusPending,
		}
		store := &stubTaskStore{task: task}
		projects := &stubProjectRepository{
			projects: []*orchestrator.Project{{ID: "proj-new"}},
		}
		svc := &TaskAppService{Tasks: store, Projects: projects}

		got, err := svc.UpdateTask("task-p", UpdateTaskRequest{ProjectID: "proj-new"})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		if got.ProjectID != "proj-new" {
			t.Fatalf("ProjectID = %q, want %q", got.ProjectID, "proj-new")
		}
	})

	t.Run("project update rejected when not pending", func(t *testing.T) {
		for _, status := range []orchestrator.TaskStatus{
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusDone,
			orchestrator.TaskStatusAborted,
		} {
			task := &orchestrator.Task{ID: "task-x", ProjectID: "proj-old", Status: status}
			store := &stubTaskStore{task: task}
			projects := &stubProjectRepository{
				projects: []*orchestrator.Project{{ID: "proj-new"}},
			}
			svc := &TaskAppService{Tasks: store, Projects: projects}

			_, err := svc.UpdateTask("task-x", UpdateTaskRequest{ProjectID: "proj-new"})
			if err == nil {
				t.Fatalf("status=%s: expected conflict error, got nil", status)
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("status=%s: expected StatusConflict, got %v", status, err)
			}
		}
	})

	t.Run("project update rejected for nonexistent project", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:        "task-p",
			ProjectID: "proj-old",
			Status:    orchestrator.TaskStatusPending,
		}
		store := &stubTaskStore{task: task}
		projects := &stubProjectRepository{projects: nil}
		svc := &TaskAppService{Tasks: store, Projects: projects}

		_, err := svc.UpdateTask("task-p", UpdateTaskRequest{ProjectID: "proj-nonexistent"})
		if err == nil {
			t.Fatal("UpdateTask() error = nil, want error")
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("expected StatusBadRequest, got %v", err)
		}
	})

	t.Run("remote_id updated", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:       "task-r",
			Title:    "t",
			RemoteID: "OLD-1",
			Status:   orchestrator.TaskStatusPending,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		newRemote := "JIRA-999"
		got, err := svc.UpdateTask("task-r", UpdateTaskRequest{RemoteID: &newRemote})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		if got.RemoteID != "JIRA-999" {
			t.Errorf("RemoteID = %q, want JIRA-999", got.RemoteID)
		}
	})

	t.Run("remote_id updated while executing", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:       "task-exec",
			Title:    "t",
			RemoteID: "OLD",
			Status:   orchestrator.TaskStatusExecuting,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		newRemote := "NEW-1"
		got, err := svc.UpdateTask("task-exec", UpdateTaskRequest{RemoteID: &newRemote})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v (should allow remote_id edit while executing)", err)
		}
		if got.RemoteID != "NEW-1" {
			t.Errorf("RemoteID = %q, want NEW-1", got.RemoteID)
		}
	})
}

func TestTaskAppServiceUpdateTask_PayloadMerge(t *testing.T) {
	t.Run("no payload preserves existing payload", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:      "task-1",
			Title:   "old title",
			Status:  orchestrator.TaskStatusPending,
			Payload: json.RawMessage(`{"artifact":{"url":"https://example.com"}}`),
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		got, err := svc.UpdateTask("task-1", UpdateTaskRequest{Title: "new title"})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, ok := m["artifact"]; !ok {
			t.Error("artifact key missing: existing payload not preserved")
		}
	})

	t.Run("payload update replaces key via shallow merge", func(t *testing.T) {
		existingPayload := json.RawMessage(`{"artifact":{"url":"old"}}`)
		task := &orchestrator.Task{
			ID:      "task-3",
			Title:   "title",
			Status:  orchestrator.TaskStatusPending,
			Payload: existingPayload,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		newPayload := json.RawMessage(`{"artifact":{"url":"new"}}`)
		got, err := svc.UpdateTask("task-3", UpdateTaskRequest{Title: "title", Payload: newPayload})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		var artifact map[string]string
		if err := json.Unmarshal(m["artifact"], &artifact); err != nil {
			t.Fatalf("unmarshal artifact: %v", err)
		}
		if artifact["url"] != "new" {
			t.Errorf("artifact url = %q, want %q", artifact["url"], "new")
		}
	})

	t.Run("payload update preserves other existing keys", func(t *testing.T) {
		existingPayload := json.RawMessage(`{"artifact":{"url":"https://example.com"},"verification":{"agent-1":{"findings":"none"}}}`)
		task := &orchestrator.Task{
			ID:      "task-4",
			Title:   "title",
			Status:  orchestrator.TaskStatusPending,
			Payload: existingPayload,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		// artifact だけ更新、verification は残す
		newPayload := json.RawMessage(`{"artifact":{"url":"https://new.example.com"}}`)
		got, err := svc.UpdateTask("task-4", UpdateTaskRequest{Title: "title", Payload: newPayload})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, ok := m["artifact"]; !ok {
			t.Error("artifact missing after payload update")
		}
		if _, ok := m["verification"]; !ok {
			t.Error("verification missing after payload update (should be preserved)")
		}
	})

	t.Run("payload containing instructions is rejected", func(t *testing.T) {
		task := &orchestrator.Task{ID: "task-5", Title: "title"}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		badPayload := json.RawMessage(`{"instructions":{"main":{"type":"execution","agent":"c"}}}`)
		_, err := svc.UpdateTask("task-5", UpdateTaskRequest{Payload: badPayload})
		if err == nil {
			t.Fatal("expected UpdateTask to reject payload containing instructions")
		}
	})

	t.Run("instructions update applied at top level", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:     "task-6",
			Title:  "title",
			Status: orchestrator.TaskStatusPending,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store, Actions: &stubActionStore{}}

		body := json.RawMessage(`[{"type":"execution","agent":"claude-code","message":"do stuff"}]`)
		got, err := svc.UpdateTask("task-6", UpdateTaskRequest{Instructions: body})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		if len(got.Instructions) == 0 {
			t.Fatal("expected instructions to be set")
		}
	})

	t.Run("instructions update is rejected while task is running", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:     "task-7",
			Title:  "title",
			Status: orchestrator.TaskStatusExecuting,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		body := json.RawMessage(`{"main":{"type":"execution","agent":"claude-code","message":"do stuff"}}`)
		_, err := svc.UpdateTask("task-7", UpdateTaskRequest{Instructions: body})
		if err == nil {
			t.Fatal("expected UpdateTask to reject instructions change while running")
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusConflict {
			t.Fatalf("expected StatusConflict, got %v", err)
		}
	})

	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	} {
		t.Run("instructions update is rejected when task is "+string(status), func(t *testing.T) {
			task := &orchestrator.Task{
				ID:     "task-instr-" + string(status),
				Title:  "title",
				Status: status,
			}
			store := &stubTaskStore{task: task}
			svc := &TaskAppService{Tasks: store}

			body := json.RawMessage(`[{"type":"execution","agent":"claude-code","message":"do stuff"}]`)
			_, err := svc.UpdateTask(task.ID, UpdateTaskRequest{Instructions: body})
			if err == nil {
				t.Fatalf("expected UpdateTask to reject instructions change when status=%s", status)
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("expected StatusConflict, got %v", err)
			}
		})
	}

	// Phase 2-3: base_branch / branch_prefix / worktree task-row updates were
	// removed. The fields no longer exist on UpdateTaskRequest, so the API
	// silently drops them (with a slog.Warn at the handler boundary). The
	// drop behavior is covered by TestTaskHandlerPatch_DeprecatedTaskRowOverridesIgnored
	// in task_patch_test.go; the per-status conflict paths once tested here
	// are obsolete.
}

func TestTaskAppServiceImportTasks_AllCreated(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	reqs := []CreateTaskRequest{
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "dev", RemoteID: "PROJ-1"},
		{ProjectID: "proj-1", Title: "Task 2", Behavior: "dev", RemoteID: "PROJ-2"},
	}
	result, err := svc.ImportTasks(reqs)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("Created = %d, want 2", result.Created)
	}
	if result.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0", result.Skipped)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want empty", result.Errors)
	}
}

func TestTaskAppServiceImportTasks_SkipsDuplicate(t *testing.T) {
	existingTask := &orchestrator.Task{
		ID:       "existing-id",
		RemoteID: "PROJ-1",
	}
	store := &stubTaskStore{
		remoteTasks: map[string]*orchestrator.Task{
			"PROJ-1": existingTask,
		},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: nil},
	}

	reqs := []CreateTaskRequest{
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "any", RemoteID: "PROJ-1"},
		{ProjectID: "proj-1", Title: "Task 2", Behavior: "any", RemoteID: "PROJ-2"},
	}
	result, err := svc.ImportTasks(reqs)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("Created = %d, want 1", result.Created)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
}

func TestTaskAppServiceImportTasks_ValidationError_BothEmpty(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: nil},
	}

	reqs := []CreateTaskRequest{
		{ProjectID: "proj-1", Title: "No Remote", Behavior: "any"},
	}
	result, err := svc.ImportTasks(reqs)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 0 {
		t.Fatalf("Created = %d, want 0", result.Created)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if result.Errors[0].Line != 1 {
		t.Fatalf("Errors[0].Line = %d, want 1", result.Errors[0].Line)
	}
}

func TestTaskAppServiceImportTasks_BehaviorError(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	reqs := []CreateTaskRequest{
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "unknown", RemoteID: "PROJ-1"},
	}
	result, err := svc.ImportTasks(reqs)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 0 {
		t.Fatalf("Created = %d, want 0", result.Created)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Errors = %d, want 1", len(result.Errors))
	}
	if result.Errors[0].Line != 1 {
		t.Fatalf("Errors[0].Line = %d, want 1", result.Errors[0].Line)
	}
	if result.Errors[0].RemoteID != "PROJ-1" {
		t.Fatalf("Errors[0].RemoteID = %q, want PROJ-1", result.Errors[0].RemoteID)
	}
}

func TestTaskAppServiceImportTasks_EmptyInput(t *testing.T) {
	svc := &TaskAppService{Tasks: &stubTaskStore{}}
	result, err := svc.ImportTasks(nil)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 0 || result.Skipped != 0 || len(result.Errors) != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestCreateTask_BehaviorFieldsExpandedToTask verifies that the behavior's
// Traits and the project-top worktree/base_branch flow onto the created Task.
// Phase 3-1 removed the behavior-level readonly/worktree/branch_prefix/
// base_branch/default_payload fields; the same task-row fields are now
// populated entirely from {project-top, canonical behavior name}.
func TestCreateTask_BehaviorFieldsExpandedToTask(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree:   true,
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Traits: []string{"artifact", "verification"},
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test task",
		Behavior:  "executor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !reflect.DeepEqual(task.Traits, []string{"artifact", "verification"}) {
		t.Errorf("Traits = %v, want %v", task.Traits, []string{"artifact", "verification"})
	}
	if task.Worktree != true {
		t.Errorf("Worktree = %v, want true", task.Worktree)
	}
	if task.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "main")
	}
}

// TestCreateTask_NoTaskRowOverridesAvailable replaces the former
// TestCreateTask_RequestOverridesTemplateFields. Phase 2-3 removed per-task
// overrides for readonly / worktree / branch_prefix / base_branch from
// CreateTaskRequest. The resulting Task must reflect the behavior template
// (and project-level defaults) verbatim — Traits is the only knob that the
// request still tweaks.
func TestCreateTask_NoTaskRowOverridesAvailable(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree:   true,
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Traits: []string{"artifact"},
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test task",
		Behavior:  "executor",
		Traits:    []string{"artifact"},
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !reflect.DeepEqual(task.Traits, []string{"artifact"}) {
		t.Errorf("Traits = %v, want %v", task.Traits, []string{"artifact"})
	}
	// Readonly / Worktree / BaseBranch come from the canonical behavior name
	// + project-top fields; the request has no knobs to override them.
	if task.Readonly != false {
		t.Errorf("Readonly = %v, want false (executor is canonically writable)", task.Readonly)
	}
	if task.Worktree != true {
		t.Errorf("Worktree = %v, want true (project-top worktree)", task.Worktree)
	}
	if task.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q (project-top base_branch)", task.BaseBranch, "main")
	}
}

func TestCreateTask_NoOverrideUsesTemplateValue(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {
				Traits: []string{"artifact"},
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test task",
		Behavior:  "executor",
		// no override fields
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !reflect.DeepEqual(task.Traits, []string{"artifact"}) {
		t.Errorf("Traits = %v, want template value %v", task.Traits, []string{"artifact"})
	}
	if task.Worktree != true {
		t.Errorf("Worktree = %v, want project-top value true", task.Worktree)
	}
}

// ---- behavior_spec tests ----

func TestTaskAppServiceCreateTask_BehaviorSpec_Success(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	// Phase 3-1: BehaviorSpec.Worktree was removed. Worktree is now governed
	// by the project-top setting; install a stub Meta carrying Worktree:true
	// so the inline behavior_spec path picks it up.
	svc.Meta = stubMetaStore{meta: &orchestrator.ProjectMeta{Worktree: true}}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "spec task",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name:   "kit/my-behavior",
			Traits: []string{"artifact"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Behavior != "kit/my-behavior" {
		t.Errorf("Behavior = %q, want %q", task.Behavior, "kit/my-behavior")
	}
	if !reflect.DeepEqual(task.Traits, []string{"artifact"}) {
		t.Errorf("Traits = %v, want [artifact]", task.Traits)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true (project-top worktree)")
	}
}

func TestTaskAppServiceCreateTask_BehaviorSpec_DefaultInstructionsMerged(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "spec task",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name:               "kit/my-behavior",
			DefaultInstruction: &orchestrator.Instruction{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "do it"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if len(task.Instructions) == 0 {
		t.Error("expected task.Instructions to be set from default_instructions")
	}
}

// Phase 3-1: BehaviorSpec.DefaultPayload was removed. The
// ValidateDefaultPayloadNoInstructions check is dead with it; the
// "instructions" guard now only applies to the wire-level payload via
// rejectPayloadInstructions, covered by other tests.

func TestTaskAppServiceCreateTask_BehaviorAndSpecMutuallyExclusive(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "bad request",
		Behavior:  "dev",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name: "kit/my-behavior",
		},
	})
	if err == nil {
		t.Fatal("CreateTask() error = nil, want error for mutually exclusive fields")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("error code = %d, want %d", se.Code, http.StatusBadRequest)
	}
	if se.Message != "behavior and behavior_spec are mutually exclusive" {
		t.Errorf("message = %q, want %q", se.Message, "behavior and behavior_spec are mutually exclusive")
	}
}

func TestTaskAppServiceCreateTask_NeitherBehaviorNorSpec_DefaultsToPlan(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: nil},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "no behavior",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil", err)
	}
	if task.Behavior != orchestrator.DefaultBehavior {
		t.Errorf("Behavior = %q, want %q", task.Behavior, orchestrator.DefaultBehavior)
	}
}

func TestTaskAppServiceCreateTask_DefaultPlan_InheritsTemplate(t *testing.T) {
	// project が supervisor behavior を template として持っているとき、
	// behavior を省略した create がそれに routing され readonly=true になる。
	// Phase 3-1 以降 readonly は behavior 名のみで決まる (template field は無い)。
	store := &stubTaskStore{}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "default to supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil", err)
	}
	if task.Behavior != orchestrator.DefaultBehavior {
		t.Errorf("Behavior = %q, want %q", task.Behavior, orchestrator.DefaultBehavior)
	}
	if !task.Readonly {
		t.Errorf("Readonly = false, want true (supervisor is canonically readonly)")
	}
}

func TestTaskAppServiceCreateTask_BehaviorSpec_NameRequired(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:    "proj-1",
		Title:        "bad request",
		BehaviorSpec: &orchestrator.BehaviorSpec{},
	})
	if err == nil {
		t.Fatal("CreateTask() error = nil, want error for missing name")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("error code = %d, want %d", se.Code, http.StatusBadRequest)
	}
	if se.Message != "behavior_spec.name is required" {
		t.Errorf("message = %q, want %q", se.Message, "behavior_spec.name is required")
	}
}

func TestTaskAppServiceImportTasks_BehaviorSpec_Success(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: nil},
	}

	reqs := []CreateTaskRequest{
		{
			ProjectID: "proj-1",
			Title:     "Spec Task",
			RemoteID:  "KIT-1",
			BehaviorSpec: &orchestrator.BehaviorSpec{
				Name:   "kit/conflict-fix",
				Traits: []string{"artifact"},
			},
		},
	}
	result, err := svc.ImportTasks(reqs)
	if err != nil {
		t.Fatalf("ImportTasks() error = %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("Created = %d, want 1", result.Created)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Errors = %v, want empty", result.Errors)
	}
	if store.createdTask == nil {
		t.Fatal("createdTask is nil")
	}
	if store.createdTask.Behavior != "kit/conflict-fix" {
		t.Errorf("Behavior = %q, want %q", store.createdTask.Behavior, "kit/conflict-fix")
	}
}

// ---- end behavior_spec tests ----

// ---- Phase 3-1: canonical readonly comes from behavior name, worktree from project-top ----

// TestCreateTask_CanonicalSupervisor_ForcesReadonly verifies that the canonical
// "supervisor" behavior is hard-wired to readonly=true.
func TestCreateTask_CanonicalSupervisor_ForcesReadonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "supervisor task",
		Behavior:  "supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !task.Readonly {
		t.Errorf("Readonly = false, want true (supervisor is canonically readonly)")
	}
}

// TestCreateTask_CanonicalExecutor_ForcesNotReadonly verifies that the canonical
// "executor" behavior is hard-wired to readonly=false.
func TestCreateTask_CanonicalExecutor_ForcesNotReadonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "executor task",
		Behavior:  "executor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Readonly {
		t.Errorf("Readonly = true, want false (executor is canonically writable)")
	}
}

// TestCreateTask_PlanAlias_ResolvesToSupervisorReadonly verifies that the legacy
// alias "plan" still works and is forced to readonly=true via the canonical
// supervisor rule.
func TestCreateTask_PlanAlias_ResolvesToSupervisorReadonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "plan-aliased task",
		Behavior:  "plan",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !task.Readonly {
		t.Errorf("Readonly = false, want true (plan alias → supervisor → readonly:true)")
	}
}

// TestCreateTask_DevAlias_ResolvesToExecutorWritable verifies that "dev" still
// works and is forced to readonly=false via the canonical executor rule.
func TestCreateTask_DevAlias_ResolvesToExecutorWritable(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "dev-aliased task",
		Behavior:  "dev",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Readonly {
		t.Errorf("Readonly = true, want false (dev alias → executor → readonly:false)")
	}
}

// TestCreateTask_NonCanonicalBehavior_NotReadonly verifies that for behaviors
// that are neither supervisor nor executor (e.g. a kit-provided custom
// behavior) the resulting Task is not readonly. P3-1 removed the per-behavior
// readonly knob, so non-canonical behaviors always default to writable.
func TestCreateTask_NonCanonicalBehavior_NotReadonly(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"impl":   {},
			"verify": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	implTask, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "impl",
		Behavior:  "impl",
	})
	if err != nil {
		t.Fatalf("CreateTask(impl) error = %v", err)
	}
	if implTask.Readonly {
		t.Errorf("impl: Readonly = true, want false (non-canonical behaviors are writable)")
	}

	verifyTask, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "verify",
		Behavior:  "verify",
	})
	if err != nil {
		t.Fatalf("CreateTask(verify) error = %v", err)
	}
	if verifyTask.Readonly {
		t.Errorf("verify: Readonly = true, want false (non-canonical behaviors are writable)")
	}
}

// TestCreateTask_ProjectLevelWorktreeTrue_AppliedToCanonicalBehavior verifies
// that the project-level worktree flag is the only source of truth for the
// canonical executor behavior in P3-1.
func TestCreateTask_ProjectLevelWorktreeTrue_AppliedToCanonicalBehavior(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "executor with project worktree",
		Behavior:  "executor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true (project-level worktree:true)")
	}
}

// TestCreateTask_ProjectLevelWorktreeUnset_ExecutorTaskIsFalse verifies that
// when the project-level worktree flag is unset, executor tasks end up with
// worktree=false. (P3-1 removed the fallback to a behavior-level value.)
func TestCreateTask_ProjectLevelWorktreeUnset_ExecutorTaskIsFalse(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		// Worktree intentionally omitted (false / unset).
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "executor without project worktree",
		Behavior:  "executor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Worktree {
		t.Errorf("Worktree = true, want false (project-level worktree unset)")
	}
}

// TestCreateTask_NonCanonicalBehavior_UsesProjectWorktree verifies that
// non-canonical behaviors take the project-level worktree value verbatim.
func TestCreateTask_NonCanonicalBehavior_UsesProjectWorktree(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"impl": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "impl with project worktree",
		Behavior:  "impl",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true (non-canonical: project-top worktree applies)")
	}
}

// ---- end Phase 3-1 ----

// ---- Phase 2-2: supervisor 3-case execution location + executor base check ----

// initServiceTestRepo creates a temporary git repo on the named branch, with
// any additional branches listed in extraBranches. HEAD stays on `branch`.
// Returns the directory or skips the test if /usr/bin/git is not available.
func initServiceTestRepo(t *testing.T, branch string, extraBranches ...string) string {
	t.Helper()
	const bin = "/usr/bin/git"
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/" + branch},
	} {
		cmd := newCmd(bin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git not available in this environment: %v\n%s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("p2-2 test"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := newCmd(bin, append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, b := range extraBranches {
		cmd := newCmd(bin, "-C", dir, "branch", b)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git branch %s: %v\n%s", b, err, out)
		}
	}
	return dir
}

// TestCreateTask_SupervisorCase1_WorktreeFalse covers the supervisor "project
// dir HEAD already matches base_branch" path: Worktree must come out false so
// the dispatcher runs the supervisor in the project dir itself.
func TestCreateTask_SupervisorCase1_WorktreeFalse(t *testing.T) {
	dir := initServiceTestRepo(t, "main")
	meta := &orchestrator.ProjectMeta{
		Worktree:   true, // project-level says "yes worktree" — case 1 must still win
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "case1 supervisor",
		Behavior:  "supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Worktree {
		t.Errorf("Worktree = true, want false (case 1 supervisor runs in project dir)")
	}
}

// TestCreateTask_SupervisorCase2_WorktreeTrue covers the supervisor case where
// the base branch exists but project HEAD is elsewhere: must allocate a
// worktree.
func TestCreateTask_SupervisorCase2_WorktreeTrue(t *testing.T) {
	dir := initServiceTestRepo(t, "feature", "main")
	meta := &orchestrator.ProjectMeta{
		Worktree:   false, // project-level off → case 2 promotes worktree anyway
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "case2 supervisor",
		Behavior:  "supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true (case 2 supervisor needs a worktree)")
	}
}

// TestCreateTask_SupervisorCase3_WorktreeTrue covers the supervisor case 3:
// base branch does not exist locally or on origin. Task creation must succeed
// (the dispatcher will create the branch) and Worktree must be true.
func TestCreateTask_SupervisorCase3_WorktreeTrue(t *testing.T) {
	dir := initServiceTestRepo(t, "main")
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "release-2026", // does not exist anywhere
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "case3 supervisor",
		Behavior:  "supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true (case 3 supervisor needs a worktree backed by a fresh base branch)")
	}
	if task.BaseBranch != "release-2026" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "release-2026")
	}
}

// TestCreateTask_ExecutorCase3_NoParent_Errors verifies that a parent-less
// executor pointed at a non-existent base_branch is rejected at creation time
// instead of waiting for the dispatcher to fail.
func TestCreateTask_ExecutorCase3_NoParent_Errors(t *testing.T) {
	dir := initServiceTestRepo(t, "main")
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "release-2026",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "case3 executor no parent",
		Behavior:  "executor",
	})
	if err == nil {
		t.Fatal("expected error for case 3 executor with no parent, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

// TestCreateTask_ExecutorCase3_WithParent_OK verifies that a child executor
// (with a parent task) is accepted even though its inherited base_branch does
// not yet exist locally — the parent supervisor is responsible for creating
// it. Parent inheritance is exercised by stubbing a parent task whose
// BaseBranch is the same missing ref.
func TestCreateTask_ExecutorCase3_WithParent_OK(t *testing.T) {
	dir := initServiceTestRepo(t, "main")
	parent := &orchestrator.Task{
		ID:         "task-parent",
		Behavior:   "supervisor",
		BaseBranch: "release-2026",
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "release-2026",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "case3 executor with parent",
		Behavior:  "executor",
		ParentID:  parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.BaseBranch != "release-2026" {
		t.Errorf("BaseBranch = %q, want %q (inherited from parent)", task.BaseBranch, "release-2026")
	}
}

// ---- P1: empty base_branch resolution ----

// TestCreateTask_EmptyBaseBranch_ExpandsCurrentBranch reproduces the mera-ui
// failure mode: project HEAD is on a feature branch and base_branch is left
// empty. P1 must resolve "" to the current branch so the supervisor ends up on
// the correct HEAD (case 1 → worktree=false when HEAD matches).
func TestCreateTask_EmptyBaseBranch_ExpandsCurrentBranch(t *testing.T) {
	dir := initServiceTestRepo(t, "feature/BGO-170")
	meta := &orchestrator.ProjectMeta{
		// BaseBranch intentionally empty → P1 must expand to current branch.
		Worktree: true,
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "mera-ui repro",
		Behavior:  "supervisor",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.BaseBranch != "feature/BGO-170" {
		t.Errorf("BaseBranch = %q, want %q (expanded from current HEAD)", task.BaseBranch, "feature/BGO-170")
	}
	// Case 1: HEAD matches baseBranch → supervisor should run in project dir.
	if task.Worktree {
		t.Errorf("Worktree = true, want false (case 1: HEAD == baseBranch)")
	}
}

// TestCreateTask_EmptyBaseBranch_DetachedHead_Returns400 verifies that a root
// task with no explicit base_branch created against a detached-HEAD project
// is rejected at creation time with a 400.
func TestCreateTask_EmptyBaseBranch_DetachedHead_Returns400(t *testing.T) {
	const bin = "/usr/bin/git"
	dir := initServiceTestRepo(t, "main")
	// Detach HEAD.
	out, err := newCmd(bin, "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("git rev-parse HEAD: %v", err)
	}
	hash := strings.TrimSpace(string(out))
	if cmd := newCmd(bin, "-C", dir, "checkout", "-q", "--detach", hash); cmd.Run() != nil {
		t.Skip("git checkout --detach: failed")
	}

	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: dir}},
	}
	_, createErr := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "detached root task",
		Behavior:  "executor",
	})
	if createErr == nil {
		t.Fatal("expected error for detached HEAD + empty base_branch, got nil")
	}
	se, ok := createErr.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", createErr)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

// ---- end P1 ----

// ---- end Phase 2-2 ----

type stubTaskStore struct {
	task        *orchestrator.Task
	tasks       map[string]*orchestrator.Task // id → task (for multi-task lookups)
	err         error
	updateCalls    int
	deleted        bool
	remoteTasks    map[string]*orchestrator.Task // remoteID → task
	createdTask    *orchestrator.Task            // captures the last created task
}

func (s *stubTaskStore) CreateTask(task *orchestrator.Task) error {
	if task.ID == "" {
		task.ID = "stub-task-id"
	}
	s.createdTask = task
	return nil
}
func (s *stubTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.tasks != nil {
		if t, ok := s.tasks[id]; ok {
			return t, nil
		}
	}
	if s.task != nil && s.task.ID == id {
		return s.task, nil
	}
	return nil, fmt.Errorf("task not found: %s", id)
}
func (s *stubTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTaskStore) UpdateTask(task *orchestrator.Task) error {
	s.updateCalls++
	return nil
}
func (s *stubTaskStore) DeleteTask(id string) error {
	s.deleted = true
	return nil
}
func (s *stubTaskStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	if s.remoteTasks != nil {
		return s.remoteTasks[remoteID], nil
	}
	return nil, nil
}
func (s *stubTaskStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTaskStore) FindDependentTasks(_ string) ([]*orchestrator.Task, error) {
	return nil, nil
}

type stubTx struct {
	updatedTask   *orchestrator.Task
	createdAction *orchestrator.Action
}

func (s *stubTx) CreateTask(task *orchestrator.Task) error { return nil }
func (s *stubTx) GetTask(id string) (*orchestrator.Task, error) {
	return nil, fmt.Errorf("not found")
}
func (s *stubTx) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTx) UpdateTask(task *orchestrator.Task) error {
	s.updatedTask = task
	return nil
}
func (s *stubTx) DeleteTask(id string) error { return nil }
func (s *stubTx) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTx) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTx) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTx) CreateAction(action *orchestrator.Action) error {
	s.createdAction = action
	return nil
}
func (s *stubTx) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) { return nil, nil }
func (s *stubTx) GetJob(id string) (*Job, error)                                  { return nil, fmt.Errorf("not found") }
func (s *stubTx) ListJobsByTask(taskID string) ([]*Job, error)                    { return nil, nil }
func (s *stubTx) UpdateJob(job *Job) error                                        { return nil }
func (s *stubTx) WithinTx(fn func(TxStore) error) error                           { return fn(s) }

type stubJobStore struct {
	job         *Job
	jobsByTask  map[string][]*Job
	getErr      error
	updateErr   error
	updateCalls int
}

func (s *stubJobStore) GetJob(id string) (*Job, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.job == nil || s.job.ID != id {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return s.job, nil
}
func (s *stubJobStore) ListJobsByTask(taskID string) ([]*Job, error) {
	if s.jobsByTask == nil {
		return nil, nil
	}
	return s.jobsByTask[taskID], nil
}
func (s *stubJobStore) UpdateJob(job *Job) error {
	s.updateCalls++
	s.job = job
	return s.updateErr
}

type stubActionStore struct {
	actions []*orchestrator.Action
}

func (s stubActionStore) CreateAction(action *orchestrator.Action) error { return nil }
func (s stubActionStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return s.actions, nil
}

type stubMetaStore struct {
	meta *orchestrator.ProjectMeta
}

func (s stubMetaStore) Get(id string) (*orchestrator.ProjectMeta, bool) {
	if s.meta == nil {
		return nil, false
	}
	return s.meta, true
}

type stubLifecycle struct {
	completedJobID    string
	unregisteredJobID string
	cleanupTaskID     string
	result            JobCompletion

	// StopJobRuntime / SignalJobRuntime are invoked from goroutines spawned
	// by CompleteJob and StopAgent, so the fields they touch must be
	// mutex-protected. Tests read them via the StoppedRuntimeID /
	// SignaledRuntimeID / SignaledSignal accessors.
	mu                sync.Mutex
	stoppedRuntimeID  string
	signaledRuntimeID string
	signaledSignal    syscall.Signal
}

func (l *stubLifecycle) CompleteJob(jobID string, result JobCompletion) {
	l.completedJobID = jobID
	l.result = result
}

func (l *stubLifecycle) UnregisterJob(jobID string) {
	l.unregisteredJobID = jobID
}

func (l *stubLifecycle) CleanupTaskWindow(taskID string) {
	l.cleanupTaskID = taskID
}

func (l *stubLifecycle) StopJobRuntime(runtimeID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stoppedRuntimeID = runtimeID
}

func (l *stubLifecycle) SignalJobRuntime(runtimeID string, sig syscall.Signal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.signaledRuntimeID = runtimeID
	l.signaledSignal = sig
}

func (l *stubLifecycle) StoppedRuntimeID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stoppedRuntimeID
}

func (l *stubLifecycle) SignaledRuntimeID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.signaledRuntimeID
}

func (l *stubLifecycle) SignaledSignal() syscall.Signal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.signaledSignal
}

func TestDuplicateTask_CopiesFields(t *testing.T) {
	source := &orchestrator.Task{
		ID:           "src-1",
		ProjectID:    "proj-1",
		Title:        "Original Task",
		Description:  "task description",
		Behavior:     "dev",
		Status:       orchestrator.TaskStatusAborted,
		Payload:      json.RawMessage(`{"old":"data"}`),
		RemoteID: "PROJ-1",
	}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{task: source}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.DuplicateTask("src-1", false)
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if task.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", task.ProjectID, "proj-1")
	}
	if task.Title != "Original Task" {
		t.Errorf("Title = %q, want %q", task.Title, "Original Task")
	}
	if task.Description != "task description" {
		t.Errorf("Description = %q, want %q", task.Description, "task description")
	}
	if task.Behavior != "dev" {
		t.Errorf("Behavior = %q, want %q", task.Behavior, "dev")
	}
	if task.RemoteID != "" {
		t.Errorf("RemoteID = %q, want empty", task.RemoteID)
	}
}

func TestDuplicateTask_InstructionsFromDefaultInstructions(t *testing.T) {
	source := &orchestrator.Task{
		ID:        "src-2",
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
		Payload:   json.RawMessage(`{"old":"data"}`),
	}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				DefaultInstruction: &orchestrator.Instruction{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Message: "do stuff"},
			},
		},
	}
	store := &stubTaskStore{task: source}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.DuplicateTask("src-2", false)
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if len(task.Instructions) == 0 {
		t.Error("instructions missing: should come from default_instructions")
	}
	if len(task.Payload) > 0 && string(task.Payload) != "{}" && string(task.Payload) != "null" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(task.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, ok := m["old"]; ok {
			t.Error("old key present: source task payload should not be copied")
		}
	}
}

func TestDuplicateTask_AutoStart(t *testing.T) {
	source := &orchestrator.Task{
		ID:        "src-3",
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusAborted,
	}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	workflow := &stubWorkflowService{}
	store := &stubTaskStore{task: source}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	_, err := svc.DuplicateTask("src-3", true)
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if workflow.appliedType != "start" {
		t.Errorf("workflow action = %q, want %q", workflow.appliedType, "start")
	}
}

func TestDuplicateTask_AnySourceStatus(t *testing.T) {
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}

	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			source := &orchestrator.Task{
				ID:        "src-1",
				ProjectID: "proj-1",
				Title:     "Task",
				Behavior:  "dev",
				Status:    status,
			}
			store := &stubTaskStore{task: source}
			svc := &TaskAppService{
				Tasks: store,
				Meta:  stubMetaStore{meta: meta},
			}
			_, err := svc.DuplicateTask("src-1", false)
			if err != nil {
				t.Fatalf("DuplicateTask() from status %s: error = %v", status, err)
			}
		})
	}
}

func TestDuplicateTask_NotFound(t *testing.T) {
	store := &stubTaskStore{err: fmt.Errorf("task not found")}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.DuplicateTask("nonexistent", false)
	if err == nil {
		t.Fatal("DuplicateTask() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

// ---- RerunTask unit tests ----

func TestRerunTask_DoneToPending(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-done",
		ProjectID: "proj-1",
		Title:     "Done Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
		Payload:   json.RawMessage(`{"artifact":{"url":"old"}}`),
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-done", RerunTaskRequest{})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if result.Status != orchestrator.TaskStatusPending {
		t.Errorf("Status = %q, want %q", result.Status, orchestrator.TaskStatusPending)
	}
	if store.updateCalls != 1 {
		t.Errorf("UpdateTask calls = %d, want 1", store.updateCalls)
	}
}

func TestRerunTask_AbortedToPendingUnit(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-aborted",
		Status:   orchestrator.TaskStatusAborted,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-aborted", RerunTaskRequest{})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if result.Status != orchestrator.TaskStatusPending {
		t.Errorf("Status = %q, want pending", result.Status)
	}
}

func TestRerunTask_WrongStatusUnit(t *testing.T) {
	wrongStatuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
	}
	for _, status := range wrongStatuses {
		t.Run(string(status), func(t *testing.T) {
			task := &orchestrator.Task{
				ID:       "task-1",
				Status:   status,
				Behavior: "dev",
			}
			store := &stubTaskStore{task: task}
			svc := &TaskAppService{Tasks: store}

			_, err := svc.RerunTask("task-1", RerunTaskRequest{})
			if err == nil {
				t.Fatalf("RerunTask() error = nil for status %q, want error", status)
			}
			se, ok := err.(*StatusError)
			if !ok || se.Code != http.StatusConflict {
				t.Fatalf("expected StatusConflict, got %v", err)
			}
		})
	}
}

func TestRerunTask_NotFoundUnit(t *testing.T) {
	store := &stubTaskStore{err: fmt.Errorf("task not found")}
	svc := &TaskAppService{Tasks: store}

	_, err := svc.RerunTask("nonexistent", RerunTaskRequest{})
	if err == nil {
		t.Fatal("RerunTask() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}

func TestRerunTask_ClearsPayloadUnit(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-1",
		Status:   orchestrator.TaskStatusDone,
		Behavior: "dev",
		Payload:  json.RawMessage(`{"artifact":{"url":"old"},"verification":{"gate":{"findings":[]}}}`),
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-1", RerunTaskRequest{})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := m["artifact"]; ok {
		t.Error("artifact should be cleared after rerun")
	}
	if _, ok := m["verification"]; ok {
		t.Error("verification should be cleared after rerun")
	}
}

func TestRerunTask_PreservesInstructionsUnit(t *testing.T) {
	task := &orchestrator.Task{
		ID:           "task-1",
		Status:       orchestrator.TaskStatusAborted,
		Behavior:     "dev",
		Payload:      json.RawMessage(`{"artifact":{"url":"old"}}`),
		Instructions: orchestrator.Instructions{{Type: orchestrator.InstructionTypeExecution, Agent: "c"}},
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-1", RerunTaskRequest{})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if len(result.Instructions) == 0 {
		t.Error("instructions should be preserved after rerun")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := m["artifact"]; ok {
		t.Error("artifact should be cleared after rerun")
	}
}

func TestRerunTask_InstructionsOverrideApplied(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-1",
		Status:   orchestrator.TaskStatusAborted,
		Behavior: "dev",
		Instructions: orchestrator.Instructions{
			{Type: orchestrator.InstructionTypeExecution, Agent: "claude-code", Model: "sonnet-4-6"},
		},
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store, Actions: &stubActionStore{}}

	override := json.RawMessage(`[{"type":"execution","agent":"claude-code","model":"opus-4-7"}]`)
	result, err := svc.RerunTask("task-1", RerunTaskRequest{InstructionsOverride: override})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	active := result.Instructions.Active()
	if active == nil || active.Model != "opus-4-7" {
		t.Errorf("expected model opus-4-7, got %#v", active)
	}
}

func TestRerunTask_AutoStartUnit(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "task-1",
		Status:   orchestrator.TaskStatusDone,
		Behavior: "dev",
	}
	workflow := &stubWorkflowService{}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{
		Tasks:    store,
		Workflow: workflow,
	}

	_, err := svc.RerunTask("task-1", RerunTaskRequest{AutoStart: true})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if workflow.appliedType != "start" {
		t.Errorf("workflow action = %q, want %q", workflow.appliedType, "start")
	}
}

func TestRerunTask_PreservesTaskMetadata(t *testing.T) {
	task := &orchestrator.Task{
		ID:          "task-meta",
		ProjectID:   "proj-1",
		Title:       "Meta Task",
		Description: "Some description",
		Behavior:    "dev",
		Status:      orchestrator.TaskStatusDone,
		Ref:         "my-ref",
		AutoStart:   true,
		Worktree:    true,
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-meta", RerunTaskRequest{})
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if result.ID != "task-meta" {
		t.Errorf("ID = %q, want %q", result.ID, "task-meta")
	}
	if result.Title != "Meta Task" {
		t.Errorf("Title = %q, want %q", result.Title, "Meta Task")
	}
	if result.Description != "Some description" {
		t.Errorf("Description = %q, want %q", result.Description, "Some description")
	}
	if result.Behavior != "dev" {
		t.Errorf("Behavior = %q, want %q", result.Behavior, "dev")
	}
	if result.Ref != "my-ref" {
		t.Errorf("Ref = %q, want %q", result.Ref, "my-ref")
	}
}

// TestGetTaskDetail_JobsIncludeWorkspacePath verifies that GetTaskDetail enriches
// returned jobs with WorkspacePath derived from RuntimesDir + RuntimeID.
// Both worktree=false and worktree=true tasks are covered; the derivation is
// identical in both cases because WorkspacePath comes from the runtime directory.
func TestGetTaskDetail_JobsIncludeWorkspacePath(t *testing.T) {
	const runtimesDir = "/data/runtimes"

	tests := []struct {
		name      string
		worktree  bool
		runtimeID string
		wantPath  string
	}{
		{
			name:      "non-worktree task with runtime",
			worktree:  false,
			runtimeID: "abc-123",
			wantPath:  "/data/runtimes/abc-123",
		},
		{
			name:      "worktree task with runtime",
			worktree:  true,
			runtimeID: "def-456",
			wantPath:  "/data/runtimes/def-456",
		},
		{
			name:      "job without runtime ID returns empty path",
			worktree:  false,
			runtimeID: "",
			wantPath:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := &orchestrator.Task{
				ID:        "task-wp",
				ProjectID: "proj-wp",
				Title:     "workspace path test",
				Status:    orchestrator.TaskStatusExecuting,
				Behavior:  "dev",
				Worktree:  tc.worktree,
			}
			job := &Job{
				ID:        "job-wp",
				TaskID:    task.ID,
				ProjectID: task.ProjectID,
				HandlerID: "main",
				Role:      "main",
				RuntimeID: tc.runtimeID,
				Status:    JobStatusRunning,
			}
			svc := &TaskAppService{
				Tasks:       &stubTaskStore{task: task},
				Actions:     stubActionStore{},
				Jobs:        &stubJobStore{jobsByTask: map[string][]*Job{task.ID: {job}}},
				RuntimesDir: runtimesDir,
			}

			got, err := svc.GetTaskDetail(task.ID)
			if err != nil {
				t.Fatalf("GetTaskDetail() error = %v", err)
			}
			if len(got.Jobs) != 1 {
				t.Fatalf("len(Jobs) = %d, want 1", len(got.Jobs))
			}
			if got.Jobs[0].WorkspacePath != tc.wantPath {
				t.Errorf("WorkspacePath = %q, want %q", got.Jobs[0].WorkspacePath, tc.wantPath)
			}
		})
	}
}


// --- stubs for ProjectAppService tests ---

type stubProjectRepository struct {
	projects []*orchestrator.Project
	listErr  error
}

func (s *stubProjectRepository) CreateProject(project *orchestrator.Project) error { return nil }
func (s *stubProjectRepository) GetProject(id string) (*orchestrator.Project, error) {
	for _, p := range s.projects {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("project not found: %s", id)
}
func (s *stubProjectRepository) ListProjects() ([]*orchestrator.Project, error) {
	return s.projects, s.listErr
}
func (s *stubProjectRepository) SetProjectWorkspace(projectID, workspaceID string) error {
	return nil
}
func (s *stubProjectRepository) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return nil, nil
}
func (s *stubProjectRepository) DeleteProject(id string) error { return nil }

type stubProjectMetaStore struct {
	metas map[string]*orchestrator.ProjectMeta
}

func (s *stubProjectMetaStore) Load(workDir string) (*orchestrator.ProjectMeta, error) {
	return nil, nil
}
func (s *stubProjectMetaStore) Get(id string) (*orchestrator.ProjectMeta, bool) {
	if s.metas == nil {
		return nil, false
	}
	m, ok := s.metas[id]
	return m, ok
}
func (s *stubProjectMetaStore) Remove(id string)                          {}
func (s *stubProjectMetaStore) LoadAll(_ []*orchestrator.Project) []error { return nil }

// TestProjectAppService_ResolveProjectRef tests all resolution priority cases.
func TestProjectAppService_ResolveProjectRef(t *testing.T) {
	// projA: id matches "uuid-001"; projD has name "uuid-001" — used to test id > name priority.
	projA := &orchestrator.Project{ID: "uuid-001", WorkDir: "/work/a"}
	projB := &orchestrator.Project{ID: "uuid-002", WorkDir: "/work/b"}
	projC := &orchestrator.Project{ID: "uuid-003", WorkDir: "/work/c"}
	projD := &orchestrator.Project{ID: "uuid-001-alias", WorkDir: "/work/d"}

	metas := map[string]*orchestrator.ProjectMeta{
		"uuid-001":       {ID: "uuid-001", Name: "Alpha Project"},
		"uuid-002":       {ID: "uuid-002", Name: "Beta Project"},
		"uuid-003":       {ID: "uuid-003", Name: "Gamma Project"},
		"uuid-001-alias": {ID: "uuid-001-alias", Name: "uuid-001"},
	}

	newSvc := func() *ProjectAppService {
		return &ProjectAppService{
			Projects: &stubProjectRepository{
				projects: []*orchestrator.Project{projA, projB, projC, projD},
			},
			Meta: &stubProjectMetaStore{metas: metas},
		}
	}

	t.Run("id exact match returns single project", func(t *testing.T) {
		got, err := newSvc().ResolveProjectRef("uuid-002")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ID != "uuid-002" {
			t.Fatalf("expected [uuid-002], got %v", got)
		}
	})

	t.Run("name exact match returns single project", func(t *testing.T) {
		got, err := newSvc().ResolveProjectRef("Beta Project")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ID != "uuid-002" {
			t.Fatalf("expected [uuid-002], got %v", got)
		}
	})

	t.Run("name partial match case-insensitive returns single project", func(t *testing.T) {
		// "GAMMA" matches "Gamma Project" only.
		got, err := newSvc().ResolveProjectRef("GAMMA")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ID != "uuid-003" {
			t.Fatalf("expected [uuid-003], got %v", got)
		}
	})

	t.Run("name partial match returns multiple candidates", func(t *testing.T) {
		// "project" matches "Alpha Project", "Beta Project", "Gamma Project" (3 of the 4).
		got, err := newSvc().ResolveProjectRef("project")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 candidates, got %d: %v", len(got), got)
		}
	})

	t.Run("no match returns 404 error", func(t *testing.T) {
		got, err := newSvc().ResolveProjectRef("nonexistent")
		if got != nil {
			t.Errorf("expected nil result, got %v", got)
		}
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		serr, ok := err.(*StatusError)
		if !ok {
			t.Fatalf("expected *StatusError, got %T: %v", err, err)
		}
		if serr.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", serr.Code)
		}
	})

	t.Run("id exact match takes priority over name exact and partial match", func(t *testing.T) {
		// "uuid-001" is projA's id AND projD's name — id match must win, returning only projA.
		got, err := newSvc().ResolveProjectRef("uuid-001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].ID != "uuid-001" {
			t.Fatalf("expected exactly [uuid-001], got %v", got)
		}
	})
}


// ---- branch variable expansion tests ----

type stubProjectLookup struct {
	project *orchestrator.Project
	err     error
}

func (s *stubProjectLookup) GetProject(id string) (*orchestrator.Project, error) {
	return s.project, s.err
}

func TestCreateTask_BehaviorBaseBranch_VariableExpanded(t *testing.T) {
	// When project.base_branch is "${current_branch}", the service calls
	// ExpandBaseBranch. With a stub workDir that is not a real git repo the
	// call should fail; here we only verify that the Projects.GetProject path
	// is reached and returns a 400 on git failure.
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "${current_branch}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: t.TempDir()}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test",
		Behavior:  "dev",
	})
	// t.TempDir() is not a git repo, so expansion must fail with a 400.
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

// Phase 2-3: TestCreateTask_RequestBaseBranch_VariableExpanded was removed.
// The behavior-level path is already covered by
// TestCreateTask_BehaviorBaseBranch_VariableExpanded above; the per-request
// override path no longer exists because CreateTaskRequest.BaseBranch was
// deleted.

func TestCreateTask_InlineBehaviorSpec_BaseBranch_VariableExpanded(t *testing.T) {
	// P3-1: BehaviorSpec no longer carries BaseBranch; the project-top
	// base_branch (with template expansion) governs both named and inline
	// behavior_spec paths.
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			BaseBranch: "${current_branch}",
		}},
		Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "proj-1", WorkDir: t.TempDir()}},
	}
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name: "myspec",
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

func TestCreateTask_DetachedHead_Returns400(t *testing.T) {
	// ProjectWorkDirLookup error should surface as 400.
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{BaseBranch: "${current_branch}", TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Projects: &stubProjectLookup{err: fmt.Errorf("project not found")},
	}
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "test",
		Behavior:  "dev",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

func TestWebAppService_ReopenTask_EmptyMessage_NilPayload(t *testing.T) {
	workflow := &stubWorkflowService{}
	svc := &WebAppService{Workflow: workflow}

	if err := svc.ReopenTask("task-1", ReopenTaskRequest{Message: ""}); err != nil {
		t.Fatalf("ReopenTask() error = %v", err)
	}
	if workflow.appliedTaskID != "task-1" {
		t.Errorf("appliedTaskID = %q, want task-1", workflow.appliedTaskID)
	}
	if workflow.appliedType != "reopen" {
		t.Errorf("appliedType = %q, want reopen", workflow.appliedType)
	}
	if workflow.appliedPayload != nil {
		t.Errorf("appliedPayload = %s, want nil", workflow.appliedPayload)
	}
}

func TestWebAppService_ReopenTask_WithMessage_HasInstructionPayload(t *testing.T) {
	workflow := &stubWorkflowService{}
	svc := &WebAppService{Workflow: workflow}

	if err := svc.ReopenTask("task-1", ReopenTaskRequest{Message: "fix review"}); err != nil {
		t.Fatalf("ReopenTask() error = %v", err)
	}
	if workflow.appliedPayload == nil {
		t.Fatal("appliedPayload should not be nil when message is non-empty")
	}
	var p struct {
		Instruction struct {
			Message string `json:"message"`
		} `json:"instruction"`
	}
	if err := json.Unmarshal(workflow.appliedPayload, &p); err != nil {
		t.Fatalf("payload unmarshal error = %v", err)
	}
	if p.Instruction.Message != "fix review" {
		t.Errorf("instruction.message = %q, want 'fix review'", p.Instruction.Message)
	}
}

// ---- Phase 1-3: dynamic base_branch (${TASK_REMOTE_ID}) tests ----

func TestCreateTask_StaticBaseBranch_PassesThrough(t *testing.T) {
	// A project with a static base_branch (no template) should be copied
	// verbatim onto the task, even when no Projects lookup is wired.
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "main",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
		// Projects intentionally nil: static base_branch must not require
		// project lookup.
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "t",
		Behavior:  "executor",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "main")
	}
}

func TestCreateTask_DynamicBaseBranch_ExpandsTaskRemoteID(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
		// Projects intentionally nil: ${TASK_REMOTE_ID} expansion is
		// independent of the project working directory.
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "t",
		Behavior:  "executor",
		RemoteID:  "PROJ-123",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.BaseBranch != "feature/PROJ-123" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "feature/PROJ-123")
	}
}

func TestCreateTask_DynamicBaseBranch_MissingRemoteID_Returns400(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "t",
		Behavior:  "executor",
		// RemoteID intentionally empty.
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

func TestCreateTask_ChildResolvesOwnBaseBranch(t *testing.T) {
	// Child resolves its own base_branch from the project template +
	// its own remote_id. The parent's resolved branch has no influence
	// (so cross-project parents do not drag their base_branch into the
	// child's project).
	parent := &orchestrator.Task{
		ID:         "parent-1",
		ProjectID:  "proj-1",
		BaseBranch: "feature/PROJ-100",
		RemoteID:   "PROJ-100",
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "child",
		Behavior:  "executor",
		RemoteID:  "PROJ-200",
		ParentID:  parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.BaseBranch != "feature/PROJ-200" {
		t.Errorf("child BaseBranch = %q, want %q (resolved from child's own remote_id)",
			task.BaseBranch, "feature/PROJ-200")
	}
}

func TestCreateTask_ChildShareesParentBranchWhenSameRemoteID(t *testing.T) {
	// To land children on the same feature branch as the parent, callers
	// pass the same remote_id from parent → child. The template + child's
	// own remote_id resolves to the same value the parent ended up with.
	parent := &orchestrator.Task{
		ID:         "parent-1",
		ProjectID:  "proj-1",
		BaseBranch: "feature/PROJ-100",
		RemoteID:   "PROJ-100",
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "child",
		Behavior:  "executor",
		RemoteID:  "PROJ-100",
		ParentID:  parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.BaseBranch != "feature/PROJ-100" {
		t.Errorf("child BaseBranch = %q, want %q (same remote_id → same branch)",
			task.BaseBranch, "feature/PROJ-100")
	}
}

func TestCreateTask_ChildInheritsParentRemoteIDByDefault(t *testing.T) {
	// A child without an explicit remote_id inherits the parent's, so the
	// project-top template expands to the same branch the parent ended up on.
	// This is what makes "spawn a child under the same Jira issue" the no-effort
	// default — callers don't have to thread remote_id through every spawn site.
	parent := &orchestrator.Task{
		ID:         "parent-1",
		ProjectID:  "proj-1",
		BaseBranch: "feature/PROJ-100",
		RemoteID:   "PROJ-100",
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "child",
		Behavior:  "executor",
		ParentID:  parent.ID,
		// RemoteID omitted: inherited from parent.
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.RemoteID != "PROJ-100" {
		t.Errorf("child RemoteID = %q, want %q (inherited from parent)", task.RemoteID, "PROJ-100")
	}
	if task.BaseBranch != "feature/PROJ-100" {
		t.Errorf("child BaseBranch = %q, want %q (template expanded with inherited remote_id)",
			task.BaseBranch, "feature/PROJ-100")
	}
}

func TestCreateTask_ChildExplicitRemoteIDOverridesParent(t *testing.T) {
	// A child that supplies its own remote_id wins over the parent's. This
	// supports the rare cross-track case where a child belongs to a different
	// Jira issue than its parent.
	parent := &orchestrator.Task{
		ID:         "parent-1",
		ProjectID:  "proj-1",
		BaseBranch: "feature/PROJ-100",
		RemoteID:   "PROJ-100",
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "child",
		Behavior:  "executor",
		RemoteID:  "PROJ-200",
		ParentID:  parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.RemoteID != "PROJ-200" {
		t.Errorf("child RemoteID = %q, want %q (explicit child value wins)", task.RemoteID, "PROJ-200")
	}
	if task.BaseBranch != "feature/PROJ-200" {
		t.Errorf("child BaseBranch = %q, want %q (resolved from explicit remote_id)",
			task.BaseBranch, "feature/PROJ-200")
	}
}

func TestCreateTask_ChildAndParentMissingRemoteID_Returns400(t *testing.T) {
	// If neither the child nor the parent supplies remote_id and the project
	// template requires ${TASK_REMOTE_ID}, there is nothing to expand and we
	// surface the usual 400.
	parent := &orchestrator.Task{
		ID:         "parent-1",
		ProjectID:  "proj-1",
		BaseBranch: "main",
		// RemoteID intentionally empty.
	}
	meta := &orchestrator.ProjectMeta{
		BaseBranch: "feature/${TASK_REMOTE_ID}",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"executor": {},
		},
	}
	store := &stubTaskStore{
		tasks: map[string]*orchestrator.Task{parent.ID: parent},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: meta},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "child",
		Behavior:  "executor",
		ParentID:  parent.ID,
		// no RemoteID anywhere.
	})
	if err == nil {
		t.Fatal("expected 400 from template expansion, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", se.Code)
	}
}

// TestTaskAppServiceCreateTask_BehaviorAlias_RequestSideResolution verifies
// that CreateTask requests with the legacy alias name ("plan" / "dev") are
// resolved to the canonical name ("supervisor" / "executor") before lookup.
// This handles older callers (CLI invocations, UI clients, persisted instructions)
// that pre-date the rename.
//
// Under Phase 3-1 the canonical behavior name dictates the readonly value
// (supervisor → true, executor → false); there is no behavior-level knob to
// disagree with.
func TestTaskAppServiceCreateTask_BehaviorAlias_RequestSideResolution(t *testing.T) {
	cases := []struct {
		name             string
		canonicalKey     string
		requestedName    string
		expectedReadonly bool
	}{
		{name: "plan request hits supervisor", canonicalKey: "supervisor", requestedName: "plan", expectedReadonly: true},
		{name: "dev request hits executor", canonicalKey: "executor", requestedName: "dev", expectedReadonly: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubTaskStore{}
			meta := &orchestrator.ProjectMeta{
				TaskBehaviors: map[string]orchestrator.TaskBehavior{
					tc.canonicalKey: {},
				},
			}
			svc := &TaskAppService{
				Tasks: store,
				Meta:  stubMetaStore{meta: meta},
			}
			task, err := svc.CreateTask(CreateTaskRequest{
				ProjectID: "proj-1",
				Title:     "via alias",
				Behavior:  tc.requestedName,
			})
			if err != nil {
				t.Fatalf("CreateTask() error = %v, want nil", err)
			}
			if task.Behavior != tc.canonicalKey {
				t.Errorf("Behavior = %q, want %q (alias must canonicalize before persist)",
					task.Behavior, tc.canonicalKey)
			}
			if task.Readonly != tc.expectedReadonly {
				t.Errorf("Readonly = %v, want %v (canonical %q decides readonly)",
					task.Readonly, tc.expectedReadonly, tc.canonicalKey)
			}
		})
	}
}
