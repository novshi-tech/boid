package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

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
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver:  stubResolver{sm: orchestrator.OneShotMachine()},
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
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver:  stubResolver{sm: orchestrator.OneShotMachine()},
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

func TestTaskAppServiceCreateTask_BehaviorNotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Transition: "one-shot"},
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
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Transition: "one-shot"},
		},
	}
	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
		Meta:    stubMetaStore{meta: meta},
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

func TestTaskAppServiceGetTaskDetail_AvailableActions_NoMeta(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-bb",
		ProjectID: "proj-bb",
		Status:    orchestrator.TaskStatusPending,
		Behavior:  "dev",
	}
	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: stubActionStore{},
		Jobs:    &stubJobStore{},
		// Meta is nil → AvailableActions should be nil/empty
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v", err)
	}
	if len(got.AvailableActions) != 0 {
		t.Errorf("AvailableActions should be empty when Meta is nil, got %v", got.AvailableActions)
	}
}

func TestTaskWorkflowServiceCompleteJobFinalizesOnResolverError(t *testing.T) {
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
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {Transition: "one-shot"}}}},
		Resolver:  stubResolver{err: fmt.Errorf("resolver failed")},
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
		orchestrator.TaskStatusInReview,
		orchestrator.TaskStatusCollectingFeedback,
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

type stubTaskStore struct {
	task        *orchestrator.Task
	err         error
	updateCalls int
	deleted     bool
}

func (s *stubTaskStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *stubTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.task == nil || s.task.ID != id {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return s.task, nil
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

type stubResolver struct {
	sm  *orchestrator.StateMachine
	err error
}

func (r stubResolver) Resolve(meta *orchestrator.ProjectMeta, behavior string) (*orchestrator.StateMachine, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.sm, nil
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
