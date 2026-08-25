package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- validateSuggestionAttr (PR #987 review, MEDIUM 9 — this function had
// no dedicated test at all before). PR-2 (docs/plans/
// suggestion-as-state-transition-impl.md §4.1) changed its signature from
// `error` to `(string, error)`: the returned verb is what parseAttrsSetPayload
// promotes into task_triage.suggestion_verb, so the one caller that already
// validates the FULL suggestion object (verb + params.wake_at) is also the
// single source of the promoted scalar — no second parse/validate pass. ----

func TestValidateSuggestionAttr_KnownVerbsAccepted(t *testing.T) {
	for _, verb := range []string{"go", "working", "park", "drop", "done", "reopen"} {
		payload, _ := json.Marshal(map[string]string{"verb": verb})
		got, err := validateSuggestionAttr(payload)
		if err != nil {
			t.Errorf("verb=%q: unexpected error: %v", verb, err)
		}
		if got != verb {
			t.Errorf("verb=%q: returned verb = %q, want %q", verb, got, verb)
		}
	}
}

func TestValidateSuggestionAttr_UnknownVerbRejected(t *testing.T) {
	for _, verb := range []string{"manual", "shape", "wake", "canonical", "totally-made-up"} {
		payload, _ := json.Marshal(map[string]string{"verb": verb})
		_, err := validateSuggestionAttr(payload)
		if err == nil {
			t.Fatalf("verb=%q: expected rejection, got success", verb)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("verb=%q: expected 400 StatusError, got %v", verb, err)
		}
	}
}

func TestValidateSuggestionAttr_MissingVerbRejected(t *testing.T) {
	_, err := validateSuggestionAttr([]byte(`{"reason":"no verb here"}`))
	if err == nil {
		t.Fatal("expected rejection for a suggestion with no verb, got success")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

// TestValidateSuggestionAttr_NullClearsWithoutValidation mirrors
// parsePromotedAttr's own null-clears-the-column convention
// (workflow_card.go) — an explicit JSON null clearing the suggestion key
// must not be rejected just because it carries no verb to validate, and
// must report the cleared ("") verb so the caller can clear the promoted
// column too.
func TestValidateSuggestionAttr_NullClearsWithoutValidation(t *testing.T) {
	verb, err := validateSuggestionAttr([]byte(`null`))
	if err != nil {
		t.Errorf("null: unexpected error: %v", err)
	}
	if verb != "" {
		t.Errorf("null: verb = %q, want empty", verb)
	}
}

func TestValidateSuggestionAttr_MalformedJSONRejected(t *testing.T) {
	if _, err := validateSuggestionAttr([]byte(`not json`)); err == nil {
		t.Fatal("expected rejection for malformed JSON, got success")
	}
}

// TestValidateSuggestionAttr_ParkWakeAt validates the RFC3339 format check on
// params.wake_at — the same format parseParkPayload's own wake_at parsing
// requires for a direct park action (workflow_card.go).
func TestValidateSuggestionAttr_ParkWakeAt(t *testing.T) {
	valid := []byte(`{"verb":"park","params":{"wake_at":"2026-09-01T00:00:00Z"}}`)
	if _, err := validateSuggestionAttr(valid); err != nil {
		t.Errorf("valid wake_at: unexpected error: %v", err)
	}

	invalid := []byte(`{"verb":"park","params":{"wake_at":"not-a-date"}}`)
	_, err := validateSuggestionAttr(invalid)
	if err == nil {
		t.Fatal("expected rejection for an invalid wake_at, got success")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

// TestValidateSuggestionAttr_ParkWakeTaskIDWithoutWakeAt confirms
// wake_task_id alone (no wake_at) is a valid park suggestion — both fields
// are independently optional (suggestionParams' own doc comment).
func TestValidateSuggestionAttr_ParkWakeTaskIDWithoutWakeAt(t *testing.T) {
	payload := []byte(`{"verb":"park","params":{"wake_task_id":"blocking-task"}}`)
	if _, err := validateSuggestionAttr(payload); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- applyParkSideEffectFromSuggestion (MEDIUM 9) ----

func TestApplyParkSideEffectFromSuggestion_WritesWakeCondition(t *testing.T) {
	txStore := &recordingTxStore{
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Kind: "issue", Urgency: "week", Detail: []byte(`{"summary":"keep me"}`)},
		},
	}
	params := suggestionParams{WakeAt: "2026-09-01T00:00:00Z", WakeTaskID: "blocking-task"}

	if err := applyParkSideEffectFromSuggestion(txStore, "t1", params); err != nil {
		t.Fatalf("applyParkSideEffectFromSuggestion: %v", err)
	}

	got := txStore.triage["t1"]
	if got.Kind != "issue" || got.Urgency != "week" || string(got.Detail) != `{"summary":"keep me"}` {
		t.Fatalf("park side effect must preserve existing kind/urgency/detail, got %+v", got)
	}
	wantWakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got.WakeAt == nil || !got.WakeAt.Equal(wantWakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wantWakeAt)
	}
	if got.WakeTaskID != "blocking-task" {
		t.Fatalf("WakeTaskID = %q, want %q", got.WakeTaskID, "blocking-task")
	}
}

// ---- accept(verb) end-to-end coverage for working/park/done (MEDIUM 9:
// design doc §3.6's "accept 適用: verb ごと" required all six; go/drop/reopen
// already had coverage — apply_action_pr3_noted_answered_test.go,
// accept_go_test.go — these three did not). ----

func TestApplyAction_Answered_AcceptWorking(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"working"}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "working"})
	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, accept working): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after accept: %+v", suggestion)
	}
}

// TestApplyAction_Answered_AcceptPark_WritesWakeCondition pins design doc
// §3.6's explicit "park params の wake 条件書き込み" requirement: the wake
// condition must come from the SUGGESTION's own params, not from the
// answered action's payload (which carries only {answer, verb, basis}).
func TestApplyAction_Answered_AcceptPark_WritesWakeCondition(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"park","params":{"wake_at":"2026-09-01T00:00:00Z","wake_task_id":"blocking-task"}}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "park"})
	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, accept park): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", result.Task.Status)
	}
	got := txStore.triage["t1"]
	wantWakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got.WakeAt == nil || !got.WakeAt.Equal(wantWakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wantWakeAt)
	}
	if got.WakeTaskID != "blocking-task" {
		t.Fatalf("WakeTaskID = %q, want %q", got.WakeTaskID, "blocking-task")
	}
	suggestion, ok := orchestrator.DetailSuggestion(got.Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after accept: %+v", suggestion)
	}
}

func TestApplyAction_Answered_AcceptDone(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"done","reason":"all children closed"}}}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": "done"})
	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(answered, accept done): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDone {
		t.Fatalf("status = %q, want done", result.Task.Status)
	}
	suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
	if ok || suggestion.Verb != "" {
		t.Errorf("suggestion still present after accept: %+v", suggestion)
	}
}

// ---- PR-3 (suggestion 状態遷移化 follow-up): 適用不能な suggestion の防御 ----
//
// The bug: card machine v2 admits only a narrow status set per verb
// (go/working/drop only from parked; park only from working; done from
// parked or working; reopen only from done/dropped — NewCardMachine's own
// doc comment). Before this PR,
// accept(verb) on a mismatched status either 409'd with sm.Apply's raw
// `no transition for action %q from status %q` (every verb except go) or,
// for go specifically, acceptGo's own pre-existing "(must be parked)"
// check — neither said what WOULD have worked. This test exercises every
// one of the 6 verbs × 4 card statuses = 24 combinations: exactly 8 succeed
// (cardTransitionAcceptEdges), the other 16 must 409 with a message naming
// the verb, the status, and (for the 5 non-go verbs, whose failure comes
// from the SAME generic sm.Apply path applyAnswered's own code shares) what
// CAN be applied instead — mirroring
// orchestrator.TestCardMachineV2_CanApplyTransitionAction_PinsExactlyEightEdges
// at the machine-rule level.
var cardTransitionAcceptEdges = map[string]map[orchestrator.TaskStatus]bool{
	"go":      {orchestrator.TaskStatusParked: true},
	"working": {orchestrator.TaskStatusParked: true},
	"drop":    {orchestrator.TaskStatusParked: true},
	"park":    {orchestrator.TaskStatusWorking: true},
	"done":    {orchestrator.TaskStatusParked: true, orchestrator.TaskStatusWorking: true},
	"reopen":  {orchestrator.TaskStatusDone: true, orchestrator.TaskStatusDropped: true},
}

func TestApplyAction_Answered_Accept_AllVerbStatusCombinations(t *testing.T) {
	verbs := []string{"go", "working", "park", "drop", "done", "reopen"}
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}

	for _, verb := range verbs {
		for _, status := range statuses {
			verb, status := verb, status
			t.Run(fmt.Sprintf("%s_from_%s", verb, status), func(t *testing.T) {
				task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: status, Card: &orchestrator.CardAttrs{}}
				detail := []byte(fmt.Sprintf(`{"attrs":{"suggestion":{"verb":%q}}}`, verb))
				txStore := &recordingTxStore{
					task:   task,
					triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", Detail: detail}},
				}
				svc := newAcceptGoWorkflowService(task, txStore, nil)

				payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerAccept, "verb": verb})
				result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})

				if cardTransitionAcceptEdges[verb][status] {
					// ---- regression: an applicable accept must still work ----
					if err != nil {
						t.Fatalf("expected success (regression), got error: %v", err)
					}
					if result == nil || result.Task == nil {
						t.Fatalf("expected a resulting task, got nil")
					}
					suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
					if ok || suggestion.Verb != "" {
						t.Errorf("suggestion still present after a valid accept: %+v", suggestion)
					}
					return
				}

				// ---- the fix: an inapplicable accept must 409 with a helpful message ----
				if err == nil {
					t.Fatalf("expected rejection for verb=%s status=%s, got success", verb, status)
				}
				se, ok := err.(*StatusError)
				if !ok || se.Code != http.StatusConflict {
					t.Fatalf("expected 409 StatusError, got %v", err)
				}
				if !strings.Contains(se.Message, verb) {
					t.Errorf("message should name the verb %q; got %q", verb, se.Message)
				}
				if !strings.Contains(se.Message, string(status)) {
					t.Errorf("message should name the status %q; got %q", status, se.Message)
				}
				// The 5 non-go verbs all fail via applyAnswered's shared generic
				// sm.Apply path (orchestrator.StateMachine.AvailableActionsHint) —
				// the message must name every action that CAN be applied from
				// this status, derived from the same rule table (never
				// hand-copied).
				if verb != "go" {
					for _, a := range orchestrator.NewCardMachine().AvailableActions(status) {
						if !strings.Contains(se.Message, a) {
							t.Errorf("verb=%s status=%s: message should mention available action %q; got %q", verb, status, a, se.Message)
						}
					}
				}
				// A failed accept must not discard the suggestion it failed to
				// apply (design doc §3.1) — it must stay in the queue so a human
				// can still Reject it.
				suggestion, ok2 := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
				if !ok2 || suggestion.Verb != verb {
					t.Errorf("suggestion should remain present after a failed accept; got %+v (present=%v)", suggestion, ok2)
				}
			})
		}
	}
}

// AvailableActionsHint's own pin (TestCardMachineV2_AvailableActionsHint_
// MatchesAvailableActions) now lives in internal/orchestrator/
// machine_card_test.go — review LOW 4 moved the hint-building itself from
// this package's local availableCardActionsHint into
// orchestrator.StateMachine.AvailableActionsHint (a single source shared
// with the Web UI's inapplicable-suggestion notice), so its own content pin
// belongs next to the method, not duplicated here. This package's own
// coverage of the hint is the end-to-end 409-message assertions in
// TestApplyAction_Answered_Accept_AllVerbStatusCombinations above.

// TestApplyAction_Answered_Reject_AllVerbStatusCombinations_AlwaysSucceeds
// pins the PR-3 decision to keep an inapplicable suggestion in the queue
// (docs/plans's own §4/§5 discussion, this PR's description "queue に載せ続
// けるか" section): if Reject were also blocked by verb/status applicability,
// an inapplicable suggestion could never be cleared at all. Unlike Accept,
// Reject applies NO verb-specific transition (workflow_card.go's
// applyAnsweredSideEffect just strips the suggestion), so it must succeed
// for every one of the 24 verb×status combinations — including all 17
// where Accept is rejected above.
func TestApplyAction_Answered_Reject_AllVerbStatusCombinations_AlwaysSucceeds(t *testing.T) {
	verbs := []string{"go", "working", "park", "drop", "done", "reopen"}
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	}

	for _, verb := range verbs {
		for _, status := range statuses {
			verb, status := verb, status
			t.Run(fmt.Sprintf("%s_from_%s", verb, status), func(t *testing.T) {
				task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: status, Card: &orchestrator.CardAttrs{}}
				detail := []byte(fmt.Sprintf(`{"attrs":{"suggestion":{"verb":%q}}}`, verb))
				txStore := &recordingTxStore{
					task:   task,
					triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", Detail: detail}},
				}
				svc := newAcceptGoWorkflowService(task, txStore, nil)

				payload, _ := json.Marshal(map[string]string{"answer": answeredAnswerReject, "verb": verb})
				result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "answered", Payload: payload})
				if err != nil {
					t.Fatalf("reject must always succeed regardless of verb/status applicability, got: %v", err)
				}
				if result.Task.Status != status {
					t.Errorf("reject must never change status; got %q, want %q", result.Task.Status, status)
				}
				suggestion, ok := orchestrator.DetailSuggestion(txStore.triage["t1"].Detail)
				if ok || suggestion.Verb != "" {
					t.Errorf("suggestion still present after reject: %+v", suggestion)
				}
			})
		}
	}
}
