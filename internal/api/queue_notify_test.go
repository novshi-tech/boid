package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type recordingNotifier struct {
	events []notify.Event
}

func (n *recordingNotifier) Notify(ctx context.Context, ev notify.Event) error {
	n.events = append(n.events, ev)
	return nil
}

func TestNotifyQueueEntryIfUrgent_FiresOnEntryWithUrgencyNow(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", Urgency: orchestrator.UrgencyNow}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}

	task := &orchestrator.Task{ID: "t1", Title: "urgent thing", Status: orchestrator.TaskStatusTriaged}
	svc.notifyQueueEntryIfUrgent(context.Background(), task, orchestrator.TaskStatusCaptured)

	if len(notifier.events) != 1 {
		t.Fatalf("events = %v, want 1 notify call", notifier.events)
	}
	if notifier.events[0].TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1", notifier.events[0].TaskID)
	}
}

func TestNotifyQueueEntryIfUrgent_SkipsNonUrgent(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t2"] = &orchestrator.TaskTriage{TaskID: "t2", Urgency: orchestrator.UrgencyToday}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}

	task := &orchestrator.Task{ID: "t2", Status: orchestrator.TaskStatusTriaged}
	svc.notifyQueueEntryIfUrgent(context.Background(), task, orchestrator.TaskStatusCaptured)

	if len(notifier.events) != 0 {
		t.Errorf("events = %v, want none (today is PR-4+ digest scope, not immediate)", notifier.events)
	}
}

func TestNotifyQueueEntryIfUrgent_SkipsWhenNotAGenuineEntry(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t3"] = &orchestrator.TaskTriage{TaskID: "t3", Urgency: orchestrator.UrgencyNow}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}

	// triaged -> ready is a within-queue transition (both already queue member
	// statuses), not a fresh entry — must not re-notify.
	task := &orchestrator.Task{ID: "t3", Status: orchestrator.TaskStatusReady}
	svc.notifyQueueEntryIfUrgent(context.Background(), task, orchestrator.TaskStatusTriaged)

	if len(notifier.events) != 0 {
		t.Errorf("events = %v, want none (not a genuine queue-entry transition)", notifier.events)
	}
}

func TestNotifyQueueEntryIfUrgent_NilNotifierIsNoop(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t4"] = &orchestrator.TaskTriage{TaskID: "t4", Urgency: orchestrator.UrgencyNow}
	svc := &TaskWorkflowService{TaskTriage: store}

	task := &orchestrator.Task{ID: "t4", Status: orchestrator.TaskStatusTriaged}
	// Must not panic.
	svc.notifyQueueEntryIfUrgent(context.Background(), task, orchestrator.TaskStatusCaptured)
}
