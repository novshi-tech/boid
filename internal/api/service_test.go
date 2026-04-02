package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestTaskWorkflowServiceCompleteJobFinalizesOnTransitionMiss(t *testing.T) {
	job := &Job{
		ID:        "job-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Status:    JobStatusRunning,
	}
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
		Behavior:  "impl",
	}

	jobs := &stubJobStore{job: job}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{task: task},
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

type stubTaskStore struct {
	task *orchestrator.Task
	err  error
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
func (s *stubTaskStore) UpdateTask(task *orchestrator.Task) error { return nil }

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
