package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- docs/plans/webui-detail-list-redesign.md PR-3: updated_at bump ----
//
// §3.2 (part B): a status-free, single-column `UPDATE tasks SET
// updated_at = ? WHERE id = ?` (tx.TouchTaskUpdatedAt) fires in the SAME Tx
// as attrs_set's suggestion-attachment side effect and as child_closed's
// parent self-record — and nowhere else attrs_set touches (observed/
// summary/urgency/link/skip/done-signal bookkeeping, noted,
// child_added/child_specced/child_dropped all stay silent, per the bump
// table in §3.2). These tests pin exactly that boundary, plus that the
// pre-existing skipTaskUpdate race defense (workflow_action.go) is
// unchanged by this PR.

// TestApplyAction_AttrsSet_ObservedOnly_DoesNotBumpUpdatedAt pins the
// negative half of the bump table: a plain bookkeeping attrs_set (khi's
// "observed" timestamp, rewritten on every judge cycle per memory:
// llm-dependent-step-is-not-convergence) must not touch updated_at at all —
// otherwise a future updated_at-sorted list (PR-4) would reorder on pure
// machine bookkeeping instead of on anything a human needs to look at.
func TestApplyAction_AttrsSet_ObservedOnly_DoesNotBumpUpdatedAt(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"observed":"2026-08-25T00:00:00Z","summary":"still investigating"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if len(txStore.touchedTaskUpdatedAtIDs) != 0 {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want none (observed/summary-only attrs_set must not bump updated_at)", txStore.touchedTaskUpdatedAtIDs)
	}
}

// TestApplyAction_AttrsSet_SuggestionAttached_BumpsUpdatedAt pins the
// positive half: a suggestion attaching (verb newly set, non-empty) IS new
// judgment material for a human — the whole reason §1's "行に描かれる動き =
// updated_at bump 集合" contract exists.
func TestApplyAction_AttrsSet_SuggestionAttached_BumpsUpdatedAt(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"go","reason":"specced children ready"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if len(txStore.touchedTaskUpdatedAtIDs) != 1 || txStore.touchedTaskUpdatedAtIDs[0] != "t1" {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want [t1]", txStore.touchedTaskUpdatedAtIDs)
	}
}

// TestApplyAction_AttrsSet_SuggestionNullClear_DoesNotBumpUpdatedAt pins
// §3.2's explicit carve-out: withdrawing a suggestion (attrs_set
// {"suggestion": null}) is not new information for a human to act on — it
// is the opposite, a suggestion going away — so it must not bump, matching
// notifySuggestionArrived's own null-check (queue_notify.go).
func TestApplyAction_AttrsSet_SuggestionNullClear_DoesNotBumpUpdatedAt(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", SuggestionVerb: "go"}}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":null}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if len(txStore.touchedTaskUpdatedAtIDs) != 0 {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want none (null-clear/withdrawal must not bump updated_at)", txStore.touchedTaskUpdatedAtIDs)
	}
	if txStore.triage["t1"].SuggestionVerb != "" {
		t.Fatalf("SuggestionVerb = %q, want cleared (sanity: the null-clear itself must still have applied)", txStore.triage["t1"].SuggestionVerb)
	}
}

// TestApplyAction_AttrsSet_ResendSameSuggestionVerb_DoesNotBumpUpdatedAt
// pins the "same 判定極性 as notifySuggestionArrived" requirement (§3.2)
// precisely: khi's own _write_suggestion (write.py) resends the IDENTICAL
// verb unconditionally on every judge cycle, unlike its diff-guarded
// _do_observed/_do_summary siblings. If bump were gated on "verb key
// present and non-empty" alone (ignoring whether it actually CHANGED), every
// one of those resends would bump updated_at too — reintroducing, for
// suggestion, exactly the "list reorders on pure machine bookkeeping" churn
// §3.2 excludes observed/summary/urgency for. Reusing suggestionVerbChanged
// (already computed for the notify gate) closes that gap for free.
func TestApplyAction_AttrsSet_ResendSameSuggestionVerb_DoesNotBumpUpdatedAt(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", SuggestionVerb: "go"}}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"go","reason":"still ready, resent this cycle"}}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if len(txStore.touchedTaskUpdatedAtIDs) != 0 {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want none (resending the identical verb must not bump updated_at)", txStore.touchedTaskUpdatedAtIDs)
	}
}

// TestApplyAction_AttrsSet_RejectsWhenConcurrentTransitionRacedAhead already
// pins (apply_action_pr4_test.go) that a raced-ahead attrs_set is rejected
// with no action recorded and the sidecar row left untouched — proof the
// skipTaskUpdate race defense itself is unchanged by this PR. This test adds
// the updated_at half of that same proof: a rejected attrs_set must not
// bump either, since tx.TouchTaskUpdatedAt now lives inside the very case
// arm that race defense guards.
func TestApplyAction_AttrsSet_RejectedByRace_DoesNotBumpUpdatedAt(t *testing.T) {
	staleTask := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	racedTask := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusAborted, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task:   racedTask,
		tasks:  map[string]*orchestrator.Task{"t1": racedTask},
		triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}},
	}
	svc := &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: staleTask},
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage: txStore,
	}

	_, err := svc.ApplyAction(context.Background(), "t1", ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"go","reason":"raced"}}`),
	})
	if err == nil {
		t.Fatal("expected rejection when a concurrent transition raced the task out of attrs_set's allowed FromStatus set")
	}
	if len(txStore.touchedTaskUpdatedAtIDs) != 0 {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want none (a rejected attrs_set must not bump updated_at)", txStore.touchedTaskUpdatedAtIDs)
	}
}

// TestFinalizeTerminal_ChildClosed_BumpsParentUpdatedAt is the child_closed
// half of §3.2's bump table: a dispatched child reaching done is new
// judgment material for the PARENT card (§2.4's "子が4回 ask を上げたが親
// card からは dispatched にしか見えなかった" symptom this whole redesign
// responds to) — so recordChildClosedOnParent's Tx (workflow_card.go) must
// bump the PARENT's updated_at, not the child's own row.
func TestFinalizeTerminal_ChildClosed_BumpsParentUpdatedAt(t *testing.T) {
	parent := &orchestrator.Task{ID: "parent-1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "p2", ParentID: "parent-1", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}

	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "child-1",
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}

	txStore := &recordingTxStore{
		task:  child,
		tasks: map[string]*orchestrator.Task{"parent-1": parent, "child-1": child},
		triage: map[string]*orchestrator.CardAttrs{
			"parent-1": {TaskID: "parent-1", Detail: detail},
		},
	}
	svc := &TaskWorkflowService{Tx: recordingTransactor{store: txStore}}

	svc.finalizeTerminal(context.Background(), child)

	if len(txStore.touchedTaskUpdatedAtIDs) != 1 || txStore.touchedTaskUpdatedAtIDs[0] != "parent-1" {
		t.Fatalf("touchedTaskUpdatedAtIDs = %v, want [parent-1]", txStore.touchedTaskUpdatedAtIDs)
	}

	// Idempotent: finalizeTerminal may be called again (e.g. CompleteJob
	// racing a retry) for the same already-closed child — matches
	// recordChildClosedOnParent's own pre-existing idempotency (its
	// `!changed` short-circuit), which this bump rides on. A repeat call
	// must not bump updated_at a second time.
	svc.finalizeTerminal(context.Background(), child)
	if len(txStore.touchedTaskUpdatedAtIDs) != 1 {
		t.Fatalf("touchedTaskUpdatedAtIDs after repeat close = %v, want still [parent-1] (must be idempotent)", txStore.touchedTaskUpdatedAtIDs)
	}
}

// TestApplyAction_AttrsSet_TouchTaskUpdatedAtFailure_FailsTheWholeAction pins
// the failure half of the bump call (Opus review round 1, PR #998, N1):
// tx.TouchTaskUpdatedAt runs INSIDE the same Tx as applyAttrsSetSideEffect
// (workflow_action.go's `case "attrs_set":` arm), so a failure there must
// fail the Tx as a whole — surfaced to the caller as a 500 StatusError, the
// same generic Tx-failure wrapping every other WithinTx error already gets
// (see ApplyAction's own `if err := s.Tx.WithinTx(...); err != nil` handling)
// — not silently swallowed or treated as a partial success. This is the
// production-DB counterpart of what a real `UPDATE tasks SET updated_at`
// failure (disk full, a lock timeout, …) would do: the whole attrs_set
// transaction rolls back with it, so the suggestion attachment
// (task_triage.suggestion_verb) that raced ahead of it never commits either
// — this recordingTxStore fake has no real rollback semantics of its own
// (unlike sqlite), so what is actually pinned here is the error-propagation
// half of that contract; the atomicity half is a property of db.InTxDB
// (internal/db), exercised for real by every sqlite-backed test elsewhere in
// this package.
func TestApplyAction_AttrsSet_TouchTaskUpdatedAtFailure_FailsTheWholeAction(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task:                  task,
		triage:                map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}},
		touchTaskUpdatedAtErr: fmt.Errorf("simulated updated_at write failure"),
	}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"suggestion":{"verb":"go","reason":"specced children ready"}}`),
	})
	if err == nil {
		t.Fatal("expected an error when tx.TouchTaskUpdatedAt fails")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 StatusError, got %v", err)
	}
	if !strings.Contains(err.Error(), "simulated updated_at write failure") {
		t.Fatalf("error = %v, want it to mention the underlying TouchTaskUpdatedAt failure", err)
	}
}
