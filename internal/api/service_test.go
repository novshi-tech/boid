package api

import (
	"fmt"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestTaskWorkflowServiceCompleteJobFinalizesOnTransitionMiss(t *testing.T) {
	job := &dispatcher.Job{
		ID:        "job-1",
		TaskID:    "task-1",
		ProjectID: "proj-1",
		Status:    dispatcher.JobStatusRunning,
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
	if got.Status != dispatcher.JobStatusCompleted {
		t.Fatalf("job status = %q, want %q", got.Status, dispatcher.JobStatusCompleted)
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

func TestTaskWorkflowServiceCompleteJobFinalizesOnResolverError(t *testing.T) {
	job := &dispatcher.Job{
		ID:        "job-2",
		TaskID:    "task-2",
		ProjectID: "proj-2",
		Status:    dispatcher.JobStatusRunning,
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
	if jobs.job.Status != dispatcher.JobStatusFailed {
		t.Fatalf("job status = %q, want %q", jobs.job.Status, dispatcher.JobStatusFailed)
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
	job         *dispatcher.Job
	getErr      error
	updateErr   error
	updateCalls int
}

func (s *stubJobStore) GetJob(id string) (*dispatcher.Job, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.job == nil || s.job.ID != id {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return s.job, nil
}
func (s *stubJobStore) ListJobsByTask(taskID string) ([]*dispatcher.Job, error) { return nil, nil }
func (s *stubJobStore) UpdateJob(job *dispatcher.Job) error {
	s.updateCalls++
	s.job = job
	return s.updateErr
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
	result            dispatcher.JobCompletionResult
}

func (l *stubLifecycle) CompleteJob(jobID string, result dispatcher.JobCompletionResult) {
	l.completedJobID = jobID
	l.result = result
}

func (l *stubLifecycle) UnregisterJob(jobID string) {
	l.unregisteredJobID = jobID
}

func (l *stubLifecycle) CleanupTaskWindow(taskID string) {
	l.cleanupTaskID = taskID
}
