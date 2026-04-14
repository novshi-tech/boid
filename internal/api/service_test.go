package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

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
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusVerifying,
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
}

func TestTaskAppServiceUpdateTask_PayloadMerge(t *testing.T) {
	t.Run("no payload preserves existing payload", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:      "task-1",
			Title:   "old title",
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

	t.Run("payload with instructions is applied", func(t *testing.T) {
		task := &orchestrator.Task{
			ID:    "task-2",
			Title: "title",
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		newPayload := json.RawMessage(`{"instructions":{"main":{"consumer":"claude-code","message":"do stuff","type":"execution"}}}`)
		got, err := svc.UpdateTask("task-2", UpdateTaskRequest{Title: "title", Payload: newPayload})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, ok := m["instructions"]; !ok {
			t.Fatal("instructions key missing from payload")
		}
	})

	t.Run("instructions top-level key is replaced by shallow merge", func(t *testing.T) {
		existingPayload := json.RawMessage(`{"instructions":{"main":{"consumer":"claude-code","message":"do stuff","type":"execution"}}}`)
		task := &orchestrator.Task{
			ID:      "task-3",
			Title:   "title",
			Payload: existingPayload,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		// top-level shallow merge: instructions キー全体が置換される
		newPayload := json.RawMessage(`{"instructions":{"rework":{"consumer":"claude-code","message":"fix stuff","type":"rework"}}}`)
		got, err := svc.UpdateTask("task-3", UpdateTaskRequest{Title: "title", Payload: newPayload})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		var instructions map[string]json.RawMessage
		if err := json.Unmarshal(m["instructions"], &instructions); err != nil {
			t.Fatalf("unmarshal instructions: %v", err)
		}
		// shallow merge では instructions top-level キーが置換されるため main は消える
		if _, ok := instructions["main"]; ok {
			t.Error("main role should be gone after shallow merge replaces instructions key")
		}
		if _, ok := instructions["rework"]; !ok {
			t.Error("rework role missing after instructions update")
		}
	})

	t.Run("payload update preserves existing artifact and verification", func(t *testing.T) {
		existingPayload := json.RawMessage(`{"artifact":{"url":"https://example.com"},"verification":{"agent-1":{"findings":"none"}}}`)
		task := &orchestrator.Task{
			ID:      "task-4",
			Title:   "title",
			Payload: existingPayload,
		}
		store := &stubTaskStore{task: task}
		svc := &TaskAppService{Tasks: store}

		// instructions だけ更新
		newPayload := json.RawMessage(`{"instructions":{"main":{"consumer":"claude-code","message":"do stuff","type":"execution"}}}`)
		got, err := svc.UpdateTask("task-4", UpdateTaskRequest{Title: "title", Payload: newPayload})
		if err != nil {
			t.Fatalf("UpdateTask() error = %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(got.Payload, &m); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if _, ok := m["artifact"]; !ok {
			t.Error("artifact missing after payload update with instructions only")
		}
		if _, ok := m["verification"]; !ok {
			t.Error("verification missing after payload update with instructions only")
		}
		if _, ok := m["instructions"]; !ok {
			t.Error("instructions missing after payload update")
		}
	})
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
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "dev", RemoteID: "PROJ-1", DataSourceID: "jira"},
		{ProjectID: "proj-1", Title: "Task 2", Behavior: "dev", RemoteID: "PROJ-2", DataSourceID: "jira"},
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
		ID:           "existing-id",
		RemoteID:     "PROJ-1",
		DataSourceID: "jira",
	}
	store := &stubTaskStore{
		remoteTasks: map[string]*orchestrator.Task{
			"PROJ-1:jira": existingTask,
		},
	}
	svc := &TaskAppService{
		Tasks: store,
		Meta:  stubMetaStore{meta: nil},
	}

	reqs := []CreateTaskRequest{
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "any", RemoteID: "PROJ-1", DataSourceID: "jira"},
		{ProjectID: "proj-1", Title: "Task 2", Behavior: "any", RemoteID: "PROJ-2", DataSourceID: "jira"},
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
		{ProjectID: "proj-1", Title: "Task 1", Behavior: "unknown", RemoteID: "PROJ-1", DataSourceID: "jira"},
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

func TestCreateTask_BehaviorFieldsExpandedToTask(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				Traits:       []string{"artifact", "verification"},
				Readonly:     false,
				Worktree:     true,
				BranchPrefix: "feature/",
				BaseBranch:   "main",
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
		Behavior:  "dev",
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
	if task.BranchPrefix != "feature/" {
		t.Errorf("BranchPrefix = %q, want %q", task.BranchPrefix, "feature/")
	}
	if task.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "main")
	}
}

func TestCreateTask_RequestOverridesTemplateFields(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				Traits:       []string{"artifact"},
				Readonly:     false,
				Worktree:     true,
				BranchPrefix: "feature/",
				BaseBranch:   "main",
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: meta},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:    "proj-1",
		Title:        "test task",
		Behavior:     "dev",
		Traits:       []string{"tasks"},
		Readonly:     boolPtr(true),
		Worktree:     boolPtr(false),
		BranchPrefix: strPtr("task/"),
		BaseBranch:   strPtr("develop"),
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !reflect.DeepEqual(task.Traits, []string{"tasks"}) {
		t.Errorf("Traits = %v, want %v", task.Traits, []string{"tasks"})
	}
	if task.Readonly != true {
		t.Errorf("Readonly = %v, want true", task.Readonly)
	}
	if task.Worktree != false {
		t.Errorf("Worktree = %v, want false", task.Worktree)
	}
	if task.BranchPrefix != "task/" {
		t.Errorf("BranchPrefix = %q, want %q", task.BranchPrefix, "task/")
	}
	if task.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q", task.BaseBranch, "develop")
	}
}

func TestCreateTask_NoOverrideUsesTemplateValue(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				Traits:   []string{"artifact"},
				Worktree: true,
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
		Behavior:  "dev",
		// no override fields
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if !reflect.DeepEqual(task.Traits, []string{"artifact"}) {
		t.Errorf("Traits = %v, want template value %v", task.Traits, []string{"artifact"})
	}
	if task.Worktree != true {
		t.Errorf("Worktree = %v, want template value true", task.Worktree)
	}
}

// ---- behavior_spec tests ----

func TestTaskAppServiceCreateTask_BehaviorSpec_Success(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "spec task",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name:     "kit/my-behavior",
			Traits:   []string{"instructions"},
			Worktree: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.Behavior != "kit/my-behavior" {
		t.Errorf("Behavior = %q, want %q", task.Behavior, "kit/my-behavior")
	}
	if !reflect.DeepEqual(task.Traits, []string{"instructions"}) {
		t.Errorf("Traits = %v, want [instructions]", task.Traits)
	}
	if !task.Worktree {
		t.Errorf("Worktree = false, want true")
	}
}

func TestTaskAppServiceCreateTask_BehaviorSpec_DefaultPayloadMerged(t *testing.T) {
	defaultPayload := `{"instructions":{"main":{"consumer":"claude-code","message":"do it"}}}`
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "spec task",
		BehaviorSpec: &orchestrator.BehaviorSpec{
			Name:           "kit/my-behavior",
			DefaultPayload: orchestrator.RawPayload(defaultPayload),
		},
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if len(task.Payload) == 0 {
		t.Error("Payload is empty, want default_payload merged")
	}
}

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

func TestTaskAppServiceCreateTask_NeitherBehaviorNorSpec(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "bad request",
	})
	if err == nil {
		t.Fatal("CreateTask() error = nil, want error for missing behavior")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Fatalf("error code = %d, want %d", se.Code, http.StatusBadRequest)
	}
	if se.Message != "either behavior or behavior_spec is required" {
		t.Errorf("message = %q, want %q", se.Message, "either behavior or behavior_spec is required")
	}
}

func TestTaskAppServiceCreateTask_BehaviorSpec_NameRequired(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "bad request",
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
			ProjectID:    "proj-1",
			Title:        "Spec Task",
			RemoteID:     "KIT-1",
			DataSourceID: "github",
			BehaviorSpec: &orchestrator.BehaviorSpec{
				Name:     "kit/conflict-fix",
				Traits:   []string{"instructions"},
				Worktree: true,
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

type stubTaskStore struct {
	task           *orchestrator.Task
	tasks          map[string]*orchestrator.Task // id → task (for multi-task lookups)
	dependentTasks []*orchestrator.Task          // returned by FindDependentTasks
	err            error
	updateCalls    int
	deleted        bool
	remoteTasks    map[string]*orchestrator.Task // "remoteID:datasourceID" → task
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
func (s *stubTaskStore) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
	if s.remoteTasks != nil {
		return s.remoteTasks[remoteID+":"+datasourceID], nil
	}
	return nil, nil
}
func (s *stubTaskStore) FindTaskByRef(ref, parentID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *stubTaskStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	return s.dependentTasks, nil
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
func (s *stubTx) FindTaskByRemote(remoteID, datasourceID string) (*orchestrator.Task, error) {
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
func (s *stubTx) WithinTx(fn func(TxStore) error) error                          { return fn(s) }

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

func TestDuplicateTask_CopiesFields(t *testing.T) {
	source := &orchestrator.Task{
		ID:           "src-1",
		ProjectID:    "proj-1",
		Title:        "Original Task",
		Description:  "task description",
		Behavior:     "dev",
		Status:       orchestrator.TaskStatusAborted,
		Payload:      json.RawMessage(`{"old":"data"}`),
		RemoteID:     "PROJ-1",
		DataSourceID: "jira",
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
	if task.DataSourceID != "" {
		t.Errorf("DataSourceID = %q, want empty", task.DataSourceID)
	}
}

func TestDuplicateTask_PayloadFromDefaultPayload(t *testing.T) {
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
				DefaultPayload: orchestrator.RawPayload(`{"instructions":{"main":{"type":"execution","consumer":"claude-code","message":"do stuff"}}}`),
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
	var m map[string]json.RawMessage
	if err := json.Unmarshal(task.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := m["instructions"]; !ok {
		t.Error("instructions key missing: payload should come from default_payload")
	}
	if _, ok := m["old"]; ok {
		t.Error("old key present: source task payload should not be copied")
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

	result, err := svc.RerunTask("task-done", false)
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

	result, err := svc.RerunTask("task-aborted", false)
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
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusVerifying,
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

			_, err := svc.RerunTask("task-1", false)
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

	_, err := svc.RerunTask("nonexistent", false)
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

	result, err := svc.RerunTask("task-1", false)
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
		ID:       "task-1",
		Status:   orchestrator.TaskStatusAborted,
		Behavior: "dev",
		Payload:  json.RawMessage(`{"instructions":{"main":{"type":"execution"}},"artifact":{"url":"old"}}`),
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-1", false)
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result.Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := m["instructions"]; !ok {
		t.Error("instructions should be preserved after rerun")
	}
	if _, ok := m["artifact"]; ok {
		t.Error("artifact should be cleared after rerun")
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

	_, err := svc.RerunTask("task-1", true)
	if err != nil {
		t.Fatalf("RerunTask() error = %v", err)
	}
	if workflow.appliedType != "start" {
		t.Errorf("workflow action = %q, want %q", workflow.appliedType, "start")
	}
}

func TestRerunTask_PreservesTaskMetadata(t *testing.T) {
	task := &orchestrator.Task{
		ID:               "task-meta",
		ProjectID:        "proj-1",
		Title:            "Meta Task",
		Description:      "Some description",
		Behavior:         "dev",
		Status:           orchestrator.TaskStatusDone,
		DependsOn:        []string{"dep-id-1"},
		DependsOnPayload: "artifact.url",
		Ref:              "my-ref",
		AutoStart:        true,
		Worktree:         true,
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{Tasks: store}

	result, err := svc.RerunTask("task-meta", false)
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
	if len(result.DependsOn) == 0 || result.DependsOn[0] != "dep-id-1" {
		t.Errorf("DependsOn = %v, want [dep-id-1]", result.DependsOn)
	}
	if result.DependsOnPayload != "artifact.url" {
		t.Errorf("DependsOnPayload = %q, want %q", result.DependsOnPayload, "artifact.url")
	}
	if result.Ref != "my-ref" {
		t.Errorf("Ref = %q, want %q", result.Ref, "my-ref")
	}
}

func TestGetTaskDetail_IncludesDependents(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-main",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	dependent := &orchestrator.Task{
		ID:        "task-dep",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
		DependsOn: []string{task.ID},
	}

	svc := &TaskAppService{
		Tasks: &stubTaskStore{
			task:           task,
			dependentTasks: []*orchestrator.Task{dependent},
		},
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}
	if len(got.Dependents) != 1 {
		t.Fatalf("Dependents len = %d, want 1", len(got.Dependents))
	}
	if got.Dependents[0].ID != dependent.ID {
		t.Fatalf("Dependents[0].ID = %q, want %q", got.Dependents[0].ID, dependent.ID)
	}
	// タスク自身に DependsOn がない場合 DependsOnResolved は nil
	if got.DependsOnResolved != nil {
		t.Fatalf("DependsOnResolved = %v, want nil", got.DependsOnResolved)
	}
}

func TestGetTaskDetail_IncludesDependsOnResolved(t *testing.T) {
	dep1 := &orchestrator.Task{
		ID:        "dep-task-1",
		ProjectID: "proj-1",
		Title:     "Dependency One",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	dep2 := &orchestrator.Task{
		ID:        "dep-task-2",
		ProjectID: "proj-1",
		Title:     "Dependency Two",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
	}
	task := &orchestrator.Task{
		ID:        "task-main",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
		DependsOn: []string{dep1.ID, dep2.ID},
	}

	svc := &TaskAppService{
		Tasks: &stubTaskStore{
			task: task,
			tasks: map[string]*orchestrator.Task{
				dep1.ID: dep1,
				dep2.ID: dep2,
			},
		},
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}
	if len(got.DependsOnResolved) != 2 {
		t.Fatalf("DependsOnResolved len = %d, want 2", len(got.DependsOnResolved))
	}
	ids := make(map[string]bool)
	for _, d := range got.DependsOnResolved {
		ids[d.ID] = true
	}
	if !ids[dep1.ID] {
		t.Errorf("DependsOnResolved missing %q", dep1.ID)
	}
	if !ids[dep2.ID] {
		t.Errorf("DependsOnResolved missing %q", dep2.ID)
	}
	// 依存するタスクがいない場合 Dependents は nil
	if got.Dependents != nil {
		t.Fatalf("Dependents = %v, want nil", got.Dependents)
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

// ---- dependency tree tests ----

// treeTestStore is a TaskStore for dependency-tree unit tests.
// FindDependentTasks returns all tasks (regardless of status) whose DependsOn contains taskID.
type treeTestStore struct {
	tasks map[string]*orchestrator.Task
}

func (s *treeTestStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *treeTestStore) GetTask(id string) (*orchestrator.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, &StatusError{Code: 404, Message: "not found: " + id}
	}
	return t, nil
}
func (s *treeTestStore) ListTasks(_ orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *treeTestStore) UpdateTask(_ *orchestrator.Task) error  { return nil }
func (s *treeTestStore) DeleteTask(_ string) error              { return nil }
func (s *treeTestStore) FindTaskByRemote(_, _ string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *treeTestStore) FindTaskByRef(_, _ string) (*orchestrator.Task, error) { return nil, nil }
func (s *treeTestStore) FindDependentTasks(taskID string) ([]*orchestrator.Task, error) {
	var result []*orchestrator.Task
	for _, t := range s.tasks {
		for _, dep := range t.DependsOn {
			if dep == taskID {
				result = append(result, t)
				break
			}
		}
	}
	return result, nil
}

func makeTreeTask(id, title string, dependsOn []string) *orchestrator.Task {
	return &orchestrator.Task{
		ID:        id,
		Title:     title,
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "dev",
		DependsOn: dependsOn,
	}
}

// TestBuildDependsOnTree_Simple verifies a simple chain: self→A→B.
func TestBuildDependsOnTree_Simple(t *testing.T) {
	taskA := makeTreeTask("a", "task-A", []string{"b"})
	taskB := makeTreeTask("b", "task-B", nil)
	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"a": taskA, "b": taskB,
	}}

	visited := map[string]bool{"self": true}
	nodes := buildDependsOnTree(store, []string{"a"}, visited)

	if len(nodes) != 1 {
		t.Fatalf("top-level nodes = %d, want 1", len(nodes))
	}
	if nodes[0].Task.ID != "a" {
		t.Errorf("nodes[0] = %q, want a", nodes[0].Task.ID)
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("nodes[0].Children = %d, want 1", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Task.ID != "b" {
		t.Errorf("nodes[0].Children[0] = %q, want b", nodes[0].Children[0].Task.ID)
	}
	if len(nodes[0].Children[0].Children) != 0 {
		t.Errorf("B should have no children, got %d", len(nodes[0].Children[0].Children))
	}
}

// TestBuildDependsOnTree_Cycle verifies that circular references do not cause infinite loops.
func TestBuildDependsOnTree_Cycle(t *testing.T) {
	taskA := makeTreeTask("a", "task-A", []string{"b"})
	taskB := makeTreeTask("b", "task-B", []string{"a"}) // circular: b → a
	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"a": taskA, "b": taskB,
	}}

	visited := map[string]bool{"self": true}
	nodes := buildDependsOnTree(store, []string{"a"}, visited)

	if len(nodes) != 1 || nodes[0].Task.ID != "a" {
		t.Fatalf("expected [a], got %v", nodes)
	}
	// B should be a child of A
	if len(nodes[0].Children) != 1 || nodes[0].Children[0].Task.ID != "b" {
		t.Fatalf("expected a.children=[b], got %v", nodes[0].Children)
	}
	// B's children: a is already visited → empty
	if len(nodes[0].Children[0].Children) != 0 {
		t.Errorf("cycle: B's children should be empty due to visited set")
	}
}

// TestBuildDependsOnTree_MultiRoot verifies multiple direct deps.
func TestBuildDependsOnTree_MultiRoot(t *testing.T) {
	taskA := makeTreeTask("a", "task-A", nil)
	taskB := makeTreeTask("b", "task-B", nil)
	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"a": taskA, "b": taskB,
	}}

	visited := map[string]bool{"self": true}
	nodes := buildDependsOnTree(store, []string{"a", "b"}, visited)

	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Task.ID != "a" || nodes[1].Task.ID != "b" {
		t.Errorf("order: got [%s, %s], want [a, b]", nodes[0].Task.ID, nodes[1].Task.ID)
	}
}

// TestBuildDependentsTree_Simple verifies a simple downstream chain: self←X←Y.
func TestBuildDependentsTree_Simple(t *testing.T) {
	self := makeTreeTask("self", "Self", nil)
	taskX := makeTreeTask("x", "task-X", []string{"self"})
	taskY := makeTreeTask("y", "task-Y", []string{"x"})
	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"self": self, "x": taskX, "y": taskY,
	}}

	visited := map[string]bool{"self": true}
	nodes := buildDependentsTree(store, "self", visited)

	if len(nodes) != 1 || nodes[0].Task.ID != "x" {
		t.Fatalf("expected [x], got %v", nodes)
	}
	if len(nodes[0].Children) != 1 || nodes[0].Children[0].Task.ID != "y" {
		t.Fatalf("expected x.children=[y], got %v", nodes[0].Children)
	}
}

// TestBuildDependentsTree_Cycle verifies cycle detection in downstream tree.
func TestBuildDependentsTree_Cycle(t *testing.T) {
	// self ← X ← Y ← self (cycle back)
	self := makeTreeTask("self", "Self", []string{"y"}) // self depends on y → creates cycle
	taskX := makeTreeTask("x", "task-X", []string{"self"})
	taskY := makeTreeTask("y", "task-Y", []string{"x"})
	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"self": self, "x": taskX, "y": taskY,
	}}

	visited := map[string]bool{"self": true}
	nodes := buildDependentsTree(store, "self", visited)

	// Should not panic or infinite loop.
	if len(nodes) == 0 {
		t.Fatal("expected at least one dependent")
	}
	// Verify self doesn't appear in its own downstream tree.
	var checkNoSelf func([]*TaskNode)
	checkNoSelf = func(ns []*TaskNode) {
		for _, n := range ns {
			if n.Task.ID == "self" {
				t.Error("'self' must not appear in its own downstream tree")
			}
			checkNoSelf(n.Children)
		}
	}
	checkNoSelf(nodes)
}

// TestGetTaskDetail_TreeFields verifies GetTaskDetail returns populated tree fields.
func TestGetTaskDetail_TreeFields(t *testing.T) {
	self := makeTreeTask("self", "Self", []string{"dep-a"})
	depA := makeTreeTask("dep-a", "Dep A", []string{"dep-b"})
	depB := makeTreeTask("dep-b", "Dep B", nil)
	downstream := makeTreeTask("down", "Downstream", []string{"self"})
	downstream.Status = orchestrator.TaskStatusPending

	store := &treeTestStore{tasks: map[string]*orchestrator.Task{
		"self": self, "dep-a": depA, "dep-b": depB, "down": downstream,
	}}
	svc := &TaskAppService{
		Tasks:   store,
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
	}

	got, err := svc.GetTaskDetail("self")
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}

	// DependsOnTree: self→dep-a→dep-b
	if len(got.DependsOnTree) != 1 || got.DependsOnTree[0].Task.ID != "dep-a" {
		t.Fatalf("DependsOnTree root: want dep-a, got %v", got.DependsOnTree)
	}
	if len(got.DependsOnTree[0].Children) != 1 || got.DependsOnTree[0].Children[0].Task.ID != "dep-b" {
		t.Fatalf("DependsOnTree depth-1: want dep-b, got %v", got.DependsOnTree[0].Children)
	}
	if len(got.DependsOnTree[0].Children[0].Children) != 0 {
		t.Errorf("dep-b should have no children")
	}

	// DependentsTree: self←downstream
	if len(got.DependentsTree) != 1 || got.DependentsTree[0].Task.ID != "down" {
		t.Fatalf("DependentsTree root: want down, got %v", got.DependentsTree)
	}
	if len(got.DependentsTree[0].Children) != 0 {
		t.Errorf("downstream should have no children")
	}
}
