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

// ---- notifySuggestionArrived (PR-2, docs/plans/
// suggestion-as-state-transition-impl.md §4.2): replaces v1's rule 4
// ("queue 入り通知") entirely. Queue membership itself is now
// suggestion-driven (store.go's "queue_next" branch, suggestion_verb != ''
// — design doc §3.6), so "a suggestion attached" IS "entered the queue" —
// there is no separate urgency-gated "entered but not urgent enough to tell
// nose yet" tier the way v1's notifyQueueEntryIfUrgent/notifyUrgencyRaised
// had. Fires whenever attrs_set's patch actually SETS a verb, regardless of
// urgency (urgency is order-only now, design doc §3.6 — gating notify on it
// would smuggle urgency back in as a decision-relevant field). ----

func TestNotifySuggestionArrived_FiresWhenVerbSet(t *testing.T) {
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{Notifier: notifier}
	task := &orchestrator.Task{ID: "t1", Title: "見積もり依頼", Status: orchestrator.TaskStatusParked}

	svc.notifySuggestionArrived(context.Background(), task, &attrsSetPatch{HasVerb: true, Verb: "park"})

	if len(notifier.events) != 1 {
		t.Fatalf("events = %v, want 1 notify call", notifier.events)
	}
	if notifier.events[0].TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1", notifier.events[0].TaskID)
	}
}

// TestNotifySuggestionArrived_FiresRegardlessOfUrgency pins the design
// decision explicitly: unlike v1's urgency=="now"-gated notify, ANY urgency
// (including none at all) still notifies once a suggestion attaches — the
// gate is "does this patch carry a verb", full stop.
func TestNotifySuggestionArrived_FiresRegardlessOfUrgency(t *testing.T) {
	for _, urgency := range []string{"", orchestrator.UrgencyToday, orchestrator.UrgencyWeek, orchestrator.UrgencySomeday, orchestrator.UrgencyNow} {
		notifier := &recordingNotifier{}
		svc := &TaskWorkflowService{Notifier: notifier}
		task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}

		svc.notifySuggestionArrived(context.Background(), task, &attrsSetPatch{HasVerb: true, Verb: "go", HasUrgency: urgency != "", Urgency: urgency})

		if len(notifier.events) != 1 {
			t.Errorf("urgency=%q: events = %v, want 1 notify call", urgency, notifier.events)
		}
	}
}

// TestNotifySuggestionArrived_SkipsWhenNoVerbSet pins the narrowness: a
// patch that doesn't carry a suggestion at all (urgency/kind-only
// attrs_set, or a nil patch) must not notify.
func TestNotifySuggestionArrived_SkipsWhenNoVerbSet(t *testing.T) {
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{Notifier: notifier}
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}

	for _, patch := range []*attrsSetPatch{
		nil,
		{},
		{HasUrgency: true, Urgency: orchestrator.UrgencyNow},
	} {
		svc.notifySuggestionArrived(context.Background(), task, patch)
	}
	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none", notifier.events)
	}
}

// TestNotifySuggestionArrived_SkipsExplicitNullClear pins that clearing a
// suggestion (attrs_set {"suggestion": null}, which still sets
// patch.HasVerb=true but with an empty Verb) does not notify — nothing new
// arrived, a suggestion was withdrawn.
func TestNotifySuggestionArrived_SkipsExplicitNullClear(t *testing.T) {
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{Notifier: notifier}
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}

	svc.notifySuggestionArrived(context.Background(), task, &attrsSetPatch{HasVerb: true, Verb: ""})

	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none for a null-clearing patch", notifier.events)
	}
}

func TestNotifySuggestionArrived_NilNotifierIsNoop(t *testing.T) {
	svc := &TaskWorkflowService{}
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	// Must not panic.
	svc.notifySuggestionArrived(context.Background(), task, &attrsSetPatch{HasVerb: true, Verb: "go"})
}

func TestNotifySuggestionArrived_NilTaskIsNoop(t *testing.T) {
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{Notifier: notifier}
	// Must not panic.
	svc.notifySuggestionArrived(context.Background(), nil, &attrsSetPatch{HasVerb: true, Verb: "go"})
	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none for a nil task", notifier.events)
	}
}
