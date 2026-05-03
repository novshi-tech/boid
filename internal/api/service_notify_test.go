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

	if err := svc.NotifyTask(context.Background(), "t1", "hello"); err != nil {
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

	if err := svc.NotifyTask(context.Background(), "t1", "hello"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.JobID != "" {
		t.Errorf("JobID = %q, want empty", notifier.event.JobID)
	}
}
