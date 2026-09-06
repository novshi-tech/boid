package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- PR #987 review, LOW 10: every direct card transition (go/working/
// park/drop/done/reopen) must strip AND record — not silently discard — an
// existing suggestion of a DIFFERENT verb, symmetrically across all six
// verbs. Before this fix only "go" ever stripped a stale suggestion at all
// (acceptGo's unconditional applyAnsweredSideEffect call), and even there
// with no audit trail; working/park/drop/done/reopen left a stale suggestion
// sitting untouched. See recordAndStripSuggestionIfPresent's own doc comment
// (workflow_card.go) for the full design. ----

func TestApplyAction_Drop_DiscardsAndRecordsExistingSuggestion(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", SuggestionVerb: "working", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"working","reason":"still active"}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "drop"})
	if err != nil {
		t.Fatalf("ApplyAction(drop): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDropped {
		t.Fatalf("status = %q, want dropped", result.Task.Status)
	}

	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after direct drop: %+v", suggestion)
	}
	// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1): every
	// path that strips the blob suggestion must also clear the promoted
	// suggestion_verb column, or the queue predicate (store.go's
	// "queue_next" branch, suggestion_verb != '') keeps showing a card whose
	// suggestion was already discarded.
	if got := txStore.triage["t1"].SuggestionVerb; got != "" {
		t.Errorf("suggestion_verb column = %q after direct drop, want cleared", got)
	}

	discard := findAction(txStore.actions, "suggestion_discarded")
	if discard == nil {
		t.Fatalf("no suggestion_discarded action recorded; actions: %+v", txStore.actions)
	}
	var payload struct {
		Verb              string `json:"verb"`
		Reason            string `json:"reason"`
		SupersedingAction string `json:"superseding_action"`
	}
	if err := json.Unmarshal(discard.Payload, &payload); err != nil {
		t.Fatalf("unmarshal suggestion_discarded payload: %v", err)
	}
	if payload.Verb != "working" || payload.Reason != "still active" || payload.SupersedingAction != "drop" {
		t.Errorf("suggestion_discarded payload = %+v, want verb=working reason='still active' superseding_action=drop", payload)
	}
}

func TestApplyAction_Park_DiscardsAndRecordsExistingSuggestion(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", SuggestionVerb: "done", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"done","reason":"all children closed"}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park"})
	if err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", result.Task.Status)
	}

	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after direct park: %+v", suggestion)
	}
	if got := txStore.triage["t1"].SuggestionVerb; got != "" {
		t.Errorf("suggestion_verb column = %q after direct park, want cleared", got)
	}
	discard := findAction(txStore.actions, "suggestion_discarded")
	if discard == nil {
		t.Fatalf("no suggestion_discarded action recorded; actions: %+v", txStore.actions)
	}
}

func TestApplyAction_NoExistingSuggestion_NoDiscardRecorded(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park"}); err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}

	if discard := findAction(txStore.actions, "suggestion_discarded"); discard != nil {
		t.Errorf("suggestion_discarded recorded with no suggestion to discard: %+v", discard)
	}
}

// TestTaskWorkflowService_AcceptGo_DiscardsAndRecordsExistingSuggestion pins
// LOW 10's symmetry requirement on "go" specifically: before this fix, a
// direct "go" click already stripped ANY existing suggestion unconditionally
// (acceptGo's own applyAnsweredSideEffect call) but never recorded it — the
// asymmetry the review flagged ("go" silently discarding, no audit trail).
func TestTaskWorkflowService_AcceptGo_DiscardsAndRecordsExistingSuggestion(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {
				TaskID:         "t1",
				SuggestionVerb: "park",
				Detail: json.RawMessage(`{
					"attrs": {"suggestion": {"verb":"park","reason":"blocked on review"}},
					"children": [{"id": "ch_00", "status": "specced", "spec": {"project": "p2", "behavior": "impl"}}]
				}`),
			},
		},
	}
	svc := newAcceptGoWorkflowService(task, txStore, &fakeTaskCreator{})

	result, err := svc.acceptGo(context.Background(), task.ID, false)
	if err != nil {
		t.Fatalf("acceptGo: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}

	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after direct go: %+v", suggestion)
	}
	if got := txStore.triage["t1"].SuggestionVerb; got != "" {
		t.Errorf("suggestion_verb column = %q after direct go, want cleared", got)
	}
	discard := findAction(txStore.actions, "suggestion_discarded")
	if discard == nil {
		t.Fatalf("no suggestion_discarded action recorded; actions: %+v", txStore.actions)
	}
	var payload struct {
		Verb   string `json:"verb"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(discard.Payload, &payload); err != nil {
		t.Fatalf("unmarshal suggestion_discarded payload: %v", err)
	}
	if payload.Verb != "park" || payload.Reason != "blocked on review" {
		t.Errorf("suggestion_discarded payload = %+v, want verb=park reason='blocked on review'", payload)
	}
}

// findAction returns the first recorded action of the given type, or nil.
func findAction(actions []*orchestrator.Action, actionType string) *orchestrator.Action {
	for _, a := range actions {
		if a.Type == actionType {
			return a
		}
	}
	return nil
}
