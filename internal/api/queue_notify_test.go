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
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Title: "見積もり依頼", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

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
	for _, urgency := range []string{"", "today", "week", "someday", "now"} {
		notifier := &recordingNotifier{}
		svc := &TaskWorkflowService{Notifier: notifier}
		task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

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
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	for _, patch := range []*attrsSetPatch{
		nil,
		{},
		{HasUrgency: true, Urgency: "now"},
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
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	svc.notifySuggestionArrived(context.Background(), task, &attrsSetPatch{HasVerb: true, Verb: ""})

	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none for a null-clearing patch", notifier.events)
	}
}

func TestNotifySuggestionArrived_NilNotifierIsNoop(t *testing.T) {
	svc := &TaskWorkflowService{}
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
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

// ---- PR #988 review, MEDIUM 3: notifySuggestionArrived itself has no
// dedup — every function-level test above calls it directly, so none of
// them would notice if the SEAM (workflow_action.go's post-Tx call, gated on
// applyAttrsSetSideEffect's own verbChanged return) were deleted or its gate
// dropped. These three exercise the full ApplyAction(attrs_set) path
// end-to-end, the same review-flagged gap: "khi's _write_suggestion sends
// unconditionally on every judge cycle" (write.py has no diff guard, unlike
// _do_summary/_do_urgency/_do_observed) — the daemon, not khi, must be the
// one place this is guarded. ----

// TestApplyAction_AttrsSet_NotifiesOnNewSuggestionVerb_ThroughApplyAction
// pins the happy path through the real seam: a brand-new suggestion (no
// task_triage row existed yet) must still notify.
func TestApplyAction_AttrsSet_NotifiesOnNewSuggestionVerb_ThroughApplyAction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	notifier := &recordingNotifier{}
	svc := newTriageWorkflowService(task, txStore)
	svc.Notifier = notifier

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"park","reason":"blocked on review"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	if len(notifier.events) != 1 {
		t.Fatalf("events = %v, want 1 notify call through the full ApplyAction seam", notifier.events)
	}
}

// TestApplyAction_AttrsSet_ResendingSameVerb_DoesNotReNotify is MEDIUM 3's
// core regression: khi re-sending the SAME verb (only reason/basis changed,
// e.g. a new Slack reply on the same open question) must not re-notify —
// without the verbChanged gate, this would fire on every single khi judge
// cycle for as long as the suggestion sits unanswered.
func TestApplyAction_AttrsSet_ResendingSameVerb_DoesNotReNotify(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", SuggestionVerb: "park", Detail: []byte(`{"attrs":{"suggestion":{"verb":"park","reason":"blocked on review"}}}`)},
	}}
	notifier := &recordingNotifier{}
	svc := newTriageWorkflowService(task, txStore)
	svc.Notifier = notifier

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"park","reason":"still blocked — new Slack reply"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none (verb unchanged — resending the same suggestion must not re-notify)", notifier.events)
	}
}

// TestApplyAction_AttrsSet_VerbChangesToADifferentVerb_Renotifies is the
// positive twin: a genuine verb change (khi withdrawing "park" in favor of
// "drop") must still notify — the gate is "did verb change", not "never
// notify twice for the same card".
func TestApplyAction_AttrsSet_VerbChangesToADifferentVerb_Renotifies(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{
		"t1": {TaskID: "t1", SuggestionVerb: "park", Detail: []byte(`{"attrs":{"suggestion":{"verb":"park"}}}`)},
	}}
	notifier := &recordingNotifier{}
	svc := newTriageWorkflowService(task, txStore)
	svc.Notifier = notifier

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"drop","reason":"turned out to be a duplicate"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	if len(notifier.events) != 1 {
		t.Fatalf("events = %v, want 1 notify call (verb genuinely changed park -> drop)", notifier.events)
	}
}

// TestApplyAttrsSetSideEffect_ReturnsVerbChanged is the unit-level pin for
// applyAttrsSetSideEffect's own new return value, isolating the "changed"
// computation itself from the ApplyAction seam tests above.
func TestApplyAttrsSetSideEffect_ReturnsVerbChanged(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		patch    *attrsSetPatch
		want     bool
	}{
		{"no verb in patch at all", "park", &attrsSetPatch{}, false},
		{"same verb resent", "park", &attrsSetPatch{HasVerb: true, Verb: "park"}, false},
		{"verb changes to a different one", "park", &attrsSetPatch{HasVerb: true, Verb: "drop"}, true},
		{"verb newly set (no prior row)", "", &attrsSetPatch{HasVerb: true, Verb: "go"}, true},
		{"explicit null clear counts as a change", "park", &attrsSetPatch{HasVerb: true, Verb: ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txStore := &recordingTxStore{triage: map[string]*orchestrator.CardAttrs{
				"t1": {TaskID: "t1", SuggestionVerb: c.existing},
			}}
			got, err := applyAttrsSetSideEffect(txStore, "t1", c.patch)
			if err != nil {
				t.Fatalf("applyAttrsSetSideEffect: %v", err)
			}
			if got != c.want {
				t.Errorf("verbChanged = %v, want %v", got, c.want)
			}
		})
	}
}
