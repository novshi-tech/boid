package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md) ----
//
// The queue predicates (queue の決定論的評価 節 rule 2/3) read
// task_triage.urgency as a REAL COLUMN — ListTasks("queue_next") orders by it
// (PR-2 demoted it from a membership gate to an ORDER BY tie-breaker, see
// store.go). Before PR-5a nothing in the daemon ever wrote that column: attrs_set folded
// everything, urgency included, into the opaque detail.attrs blob. The queue
// view was therefore permanently empty and notify could never fire. These
// tests pin the promotion that closes it.

// TestApplyAction_AttrsSet_PromotesUrgencyToColumn pins that an urgency sent
// via attrs_set lands in the queue predicate's column, not only in the opaque
// blob.
func TestApplyAction_AttrsSet_PromotesUrgencyToColumn(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"today","kind":"issue","summary":"見積もり依頼"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected a task_triage row")
	}
	if tt.Urgency != "today" {
		t.Fatalf("urgency column = %q, want %q (queue_next joins on this column)", tt.Urgency, "today")
	}
	if tt.Kind != "issue" {
		t.Fatalf("kind column = %q, want %q", tt.Kind, "issue")
	}

	// The remaining keys still fold into the opaque blob untouched.
	var detail struct {
		Attrs map[string]json.RawMessage `json:"attrs"`
	}
	if err := json.Unmarshal(tt.Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if string(detail.Attrs["summary"]) != `"見積もり依頼"` {
		t.Fatalf("summary not folded into detail.attrs: %s", tt.Detail)
	}
	// urgency/kind are promoted, not duplicated: a second copy in the blob
	// could drift from the column that actually drives the queue.
	if _, dup := detail.Attrs["urgency"]; dup {
		t.Fatalf("urgency duplicated into detail.attrs as well as the column: %s", tt.Detail)
	}
	if _, dup := detail.Attrs["kind"]; dup {
		t.Fatalf("kind duplicated into detail.attrs as well as the column: %s", tt.Detail)
	}
}

// TestApplyAction_AttrsSet_RejectsUnknownUrgency pins vocabulary validation on
// the two promoted keys. Unlike the opaque blob (whose keys the daemon
// deliberately does not interpret — 逆輸入3), urgency and kind ARE daemon
// vocabulary: urgency drives a SQL predicate, so a typo would silently drop
// the card out of the queue forever with no error anywhere.
func TestApplyAction_AttrsSet_RejectsUnknownUrgency(t *testing.T) {
	for _, payload := range []string{
		`{"urgency":"urgent"}`,
		`{"urgency":123}`,
		`{"kind":"epic"}`,
	} {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task}
		svc := newTriageWorkflowService(task, txStore)

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "attrs_set", Payload: []byte(payload)})
		if err == nil {
			t.Fatalf("payload %s: expected a 400, got success", payload)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 400 {
			t.Fatalf("payload %s: err = %v, want a 400 StatusError", payload, err)
		}
		// PR-B: newTriageWorkflowService now seeds an EMPTY task_triage row
		// up front (machineFor needs one present to pick NewCardMachine), so
		// a nil check can no longer distinguish "rejected before any write"
		// from "row existed all along" — assert the row's fold-relevant
		// fields are still at their zero value instead (still nothing
		// written), which is what this test actually cares about.
		if tt := txStore.triage["t1"]; tt == nil || tt.Urgency != "" || tt.Kind != "" || len(tt.Detail) != 0 {
			t.Fatalf("payload %s: rejected attrs_set wrote into the sidecar row, got %+v", payload, tt)
		}
	}
}

// TestApplyAction_AttrsSet_UrgencyNullClears pins that an explicit JSON null
// clears the column (khi walking a card back to "no urgency" must be
// expressible — otherwise urgency would be a one-way ratchet at the daemon
// level, which is policy the daemon deliberately does not own).
func TestApplyAction_AttrsSet_UrgencyNullClears(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{
		"t1": {TaskID: "t1", Urgency: "now"},
	}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":null}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if got := txStore.triage["t1"].Urgency; got != "" {
		t.Fatalf("urgency = %q after explicit null, want cleared", got)
	}
}

// ---- PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1) ----
//
// suggestion_verb is promoted to a real task_triage column the same way
// urgency/kind were in Phase 1 PR-5a above: the queue predicate
// (store.go's "queue_next" branch) now reads suggestion_verb directly, so a
// suggestion that only ever reached the opaque blob would be invisible to
// the queue — exactly the bug PR-5a fixed for urgency.

// TestApplyAction_AttrsSet_PromotesSuggestionVerbToColumn pins that a
// suggestion sent via attrs_set lands in the promoted column, while the full
// suggestion object (verb+reason+params) stays in the opaque blob too —
// unlike urgency/kind, suggestion is NOT removed from the blob (display
// still reads orchestrator.DetailSuggestion off the blob; the column exists
// purely for the SQL predicate, see applyAttrsSetSideEffect's doc comment).
func TestApplyAction_AttrsSet_PromotesSuggestionVerbToColumn(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"park","reason":"blocked on review"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected a task_triage row")
	}
	if tt.SuggestionVerb != "park" {
		t.Fatalf("suggestion_verb column = %q, want %q (queue_next reads this column)", tt.SuggestionVerb, "park")
	}
	suggestion, ok := orchestrator.DetailSuggestion(tt.Detail)
	if !ok || suggestion.Verb != "park" || suggestion.Reason != "blocked on review" {
		t.Fatalf("blob suggestion = %+v (ok=%v), want the full object still readable off the blob", suggestion, ok)
	}
}

// TestApplyAction_AttrsSet_RejectsUnknownSuggestionVerb mirrors
// TestApplyAction_AttrsSet_RejectsUnknownUrgency: suggestion.verb is boid's
// own closed state-machine vocabulary (orchestrator.IsCardTransitionAction),
// not an opaque workspace value, so an unrecognized verb must 400 rather
// than silently writing a column the queue predicate can never match on
// (this end-to-end path already had unit coverage via
// TestValidateSuggestionAttr_UnknownVerbRejected — this pins the same
// rejection through the full ApplyAction/attrs_set entry point).
func TestApplyAction_AttrsSet_RejectsUnknownSuggestionVerb(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"manual"}}`),
	})
	if err == nil {
		t.Fatal("expected a 400, got success")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
	if tt := txStore.triage["t1"]; tt != nil && tt.SuggestionVerb != "" {
		t.Fatalf("rejected suggestion still wrote suggestion_verb: %+v", tt)
	}
}

// TestApplyAction_AttrsSet_SuggestionNullClearsColumn mirrors
// TestApplyAction_AttrsSet_UrgencyNullClears: an explicit JSON null clears
// suggestion_verb the same way it already clears the blob's suggestion key
// (validateSuggestionAttr's own null-clears convention).
func TestApplyAction_AttrsSet_SuggestionNullClearsColumn(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{
		"t1": {TaskID: "t1", SuggestionVerb: "go", Detail: json.RawMessage(`{"attrs":{"suggestion":{"verb":"go"}}}`)},
	}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":null}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if got := txStore.triage["t1"].SuggestionVerb; got != "" {
		t.Fatalf("suggestion_verb = %q after explicit null, want cleared", got)
	}
}

// TestCreateTask_PreExecutionSeedsTriageRow pins the invariant PR-5a's list
// predicate rests on: a task created directly into a pre-execution status IS a
// triage task and gets its sidecar row at birth. Without this, khi's freshly
// ingested cards would be invisible to ListTriage until their first attrs_set
// happened to land. v2 (docs/plans/suggestion-as-state-transition-impl.md §3.5)
// folds captured/triaged into a single "parked" initial_status value.
func TestCreateTask_PreExecutionSeedsTriageRow(t *testing.T) {
	triage := &stubTriageStore{}
	svc := newInitialStatusTestService()
	svc.TaskTriage = triage

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "見積もり依頼",
		Behavior:      "triage",
		InitialStatus: "parked",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if triage.rows[task.ID] == nil {
		t.Fatalf("no task_triage row seeded for a parked task (rows=%v)", triage.rows)
	}
}

// TestCreateTask_OrdinaryTaskSeedsNoTriageRow is the other half of the
// invariant: an ordinary (pending) task must NOT get a sidecar row, or the
// "has a row = is a triage task" discriminator collapses and every ingest /
// sweep executor task would show up in the queue listing.
func TestCreateTask_OrdinaryTaskSeedsNoTriageRow(t *testing.T) {
	triage := &stubTriageStore{}
	svc := newInitialStatusTestService()
	svc.TaskTriage = triage

	task, err := svc.CreateTask(CreateTaskRequest{ProjectID: "proj-1", Title: "ingest", Behavior: "triage"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if triage.rows[task.ID] != nil {
		t.Fatalf("ordinary pending task got a task_triage row: %+v", triage.rows[task.ID])
	}
}
