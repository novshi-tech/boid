package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type capturingNotifier struct {
	event notify.Event
	err   error
}

func (n *capturingNotifier) Notify(_ context.Context, ev notify.Event) error {
	n.event = ev
	return n.err
}

func TestNotifyTask_InteractiveRunningJobSetsJobID(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	jobs := []*Job{
		{ID: "j1", TaskID: "t1", Status: JobStatusCompleted, Interactive: false},
		{ID: "j2", TaskID: "t1", Status: JobStatusRunning, Interactive: true},
	}
	notifier := &capturingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{task: task},
		Jobs:   &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify: notifier,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.JobID != "j2" {
		t.Errorf("JobID = %q, want %q", notifier.event.JobID, "j2")
	}
}

func TestNotifyTask_NoInteractiveRunningJob_JobIDEmpty(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	jobs := []*Job{
		{ID: "j1", TaskID: "t1", Status: JobStatusCompleted, Interactive: true},
		{ID: "j2", TaskID: "t1", Status: JobStatusRunning, Interactive: false},
	}
	notifier := &capturingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{task: task},
		Jobs:   &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify: notifier,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.JobID != "" {
		t.Errorf("JobID = %q, want empty", notifier.event.JobID)
	}
}

func TestNotifyTask_AskMode_TransitionsToAwaiting(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "Plan ready", "Approve?", "q-1"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.Message != "Plan ready" {
		t.Errorf("message = %q, want Plan ready", notifier.event.Message)
	}
	if workflow.appliedType != "ask" {
		t.Errorf("applied action type = %q, want ask", workflow.appliedType)
	}
}

func TestAnswerTask_TransitionsToExecuting(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Behavior:  "dev",
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Workflow: workflow,
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}
	if workflow.appliedType != "answer" {
		t.Errorf("applied action type = %q, want answer", workflow.appliedType)
	}
}

func TestAnswerTask_NotAwaiting_ReturnsConflict(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t1",
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
	}

	err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != 409 {
		t.Errorf("expected StatusError 409, got %v", err)
	}
}
