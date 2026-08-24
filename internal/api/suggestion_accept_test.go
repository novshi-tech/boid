package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- validateSuggestionAttr (PR #987 review, MEDIUM 9 — this function had
// no dedicated test at all before). ----

func TestValidateSuggestionAttr_KnownVerbsAccepted(t *testing.T) {
	for _, verb := range []string{"go", "working", "park", "drop", "done", "reopen"} {
		payload, _ := json.Marshal(map[string]string{"verb": verb})
		if err := validateSuggestionAttr(payload); err != nil {
			t.Errorf("verb=%q: unexpected error: %v", verb, err)
		}
	}
}

func TestValidateSuggestionAttr_UnknownVerbRejected(t *testing.T) {
	for _, verb := range []string{"manual", "shape", "wake", "canonical", "totally-made-up"} {
		payload, _ := json.Marshal(map[string]string{"verb": verb})
		err := validateSuggestionAttr(payload)
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
	err := validateSuggestionAttr([]byte(`{"reason":"no verb here"}`))
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
// (workflow_triage.go) — an explicit JSON null clearing the suggestion key
// must not be rejected just because it carries no verb to validate.
func TestValidateSuggestionAttr_NullClearsWithoutValidation(t *testing.T) {
	if err := validateSuggestionAttr([]byte(`null`)); err != nil {
		t.Errorf("null: unexpected error: %v", err)
	}
}

func TestValidateSuggestionAttr_MalformedJSONRejected(t *testing.T) {
	if err := validateSuggestionAttr([]byte(`not json`)); err == nil {
		t.Fatal("expected rejection for malformed JSON, got success")
	}
}

// TestValidateSuggestionAttr_ParkWakeAt validates the RFC3339 format check on
// params.wake_at — the same format parseParkPayload's own wake_at parsing
// requires for a direct park action (workflow_triage.go).
func TestValidateSuggestionAttr_ParkWakeAt(t *testing.T) {
	valid := []byte(`{"verb":"park","params":{"wake_at":"2026-09-01T00:00:00Z"}}`)
	if err := validateSuggestionAttr(valid); err != nil {
		t.Errorf("valid wake_at: unexpected error: %v", err)
	}

	invalid := []byte(`{"verb":"park","params":{"wake_at":"not-a-date"}}`)
	err := validateSuggestionAttr(invalid)
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
	if err := validateSuggestionAttr(payload); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---- applyParkSideEffectFromSuggestion (MEDIUM 9) ----

func TestApplyParkSideEffectFromSuggestion_WritesWakeCondition(t *testing.T) {
	txStore := &recordingTxStore{
		triage: map[string]*orchestrator.TaskTriage{
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
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
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.TaskTriage{
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
