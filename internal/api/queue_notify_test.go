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

// TestNotifyUrgencyRaised_FiresForAlreadyQueueMember pins rule 4's second
// entry point. The order khi actually uses — create captured → `triage` →
// `attrs_set {"urgency":"now"}` — puts the urgency on a card that is ALREADY a
// queue member, which the entry-transition detector cannot see (attrs_set is
// non-transitioning, so fromStatus == status). Without this the natural
// ingestion order silently produced no notification at all.
func TestNotifyUrgencyRaised_FiresForAlreadyQueueMember(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", Urgency: orchestrator.UrgencyNow}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}

	task := &orchestrator.Task{ID: "t1", Title: "urgent thing", Status: orchestrator.TaskStatusTriaged}

	// The entry detector stays silent for a within-queue "transition".
	svc.notifyQueueEntryIfUrgent(context.Background(), task, orchestrator.TaskStatusTriaged)
	if len(notifier.events) != 0 {
		t.Fatalf("entry detector fired for a non-entry: %v", notifier.events)
	}

	svc.notifyUrgencyRaised(context.Background(), task, &attrsSetPatch{HasUrgency: true, Urgency: orchestrator.UrgencyNow})
	if len(notifier.events) != 1 {
		t.Fatalf("events = %v, want 1 notify call", notifier.events)
	}
}

// TestNotifyUrgencyRaised_SkipsUnrelatedPatches pins the narrowness: an
// attrs_set that doesn't set urgency to "now" must not re-notify on every khi
// sweep.
func TestNotifyUrgencyRaised_SkipsUnrelatedPatches(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", Urgency: orchestrator.UrgencyNow}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusTriaged}

	for _, patch := range []*attrsSetPatch{
		nil,
		{}, // summary-only attrs_set
		{HasUrgency: true, Urgency: orchestrator.UrgencyToday}, // not the "now" tier
	} {
		svc.notifyUrgencyRaised(context.Background(), task, patch)
	}
	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none", notifier.events)
	}
}

// TestNotifyUrgencyRaised_SkipsNonQueueMember pins that a working card (past
// the queue) does not notify.
func TestNotifyUrgencyRaised_SkipsNonQueueMember(t *testing.T) {
	store := newSweepFakeStore()
	store.triage["t1"] = &orchestrator.TaskTriage{TaskID: "t1", Urgency: orchestrator.UrgencyNow}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{TaskTriage: store, Notifier: notifier}
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusWorking}

	svc.notifyUrgencyRaised(context.Background(), task, &attrsSetPatch{HasUrgency: true, Urgency: orchestrator.UrgencyNow})
	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none for a working card", notifier.events)
	}
}
