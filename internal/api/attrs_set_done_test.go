package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): I-5b の service 層ガード ----
//
// machine.go's own rule table has NO attrs_set rule for FromStatus=="done"
// (preExecutionStatuses, machine.go) — on purpose: adding one there would let
// attrs_set land on ANY done task, triage or not, breaking 論点6-3 ("通常
// task の done に発火させない"). Instead ApplyAction's service layer allows
// attrs_set into a done task ONLY when the task carries a task_triage
// sidecar row (12節 B-6 既定案の判定 key) — see
// resolveAttrsSetDoneTransition's own doc comment (workflow_action.go).
//
// These tests do not assert on the I-5c log line itself (this codebase's
// existing sweep tests — queue_sweep_test.go — likewise never assert on
// slog output, only on returned/stored state): I-5c's visibility is pinned
// structurally here by confirming the write actually lands (the log call is
// unconditionally reached on that same success path).

// newDoneTriageWorkflowService is newTriageWorkflowService
// (apply_action_phase1_test.go) plus TaskTriage wired to the SAME
// recordingTxStore, so the pre-Tx I-5b guard (which reads s.TaskTriage, not
// tx.GetTaskTriage) can see rows the test seeds into txStore.triage.
// newTriageWorkflowService itself is left untouched (many other tests rely
// on TaskTriage staying nil) — this is a parallel constructor, not a shared
// one.
func newDoneTriageWorkflowService(task *orchestrator.Task, txStore *recordingTxStore) *TaskWorkflowService {
	svc := newTriageWorkflowService(task, txStore)
	svc.TaskTriage = txStore
	return svc
}

func TestApplyAction_AttrsSet_Done_WithTaskTriageRow_Allowed(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{"existing":"keep"}`)}
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: []byte(`{"attrs":{"observed":{"source_closed":true}}}`)}},
	}
	svc := newDoneTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"observed":{"source_closed":false}}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(attrs_set on done triage task): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDone {
		t.Fatalf("status = %q, want unchanged (done) — attrs_set must stay non-transitioning even on this path", result.Task.Status)
	}
	if string(result.Task.Payload) != `{"existing":"keep"}` {
		t.Fatalf("task.Payload = %s, want untouched (attrs_set never merges into task.Payload)", result.Task.Payload)
	}
	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected task_triage row to still exist")
	}
	if orchestrator.SourceClosed(tt.Detail) {
		t.Fatalf("detail after fold still reports source_closed=true, want false (the patch just set it)")
	}
	if txStore.updatedTask != nil {
		t.Fatalf("attrs_set called tx.UpdateTask (updatedTask=%+v) even though it is non-transitioning — same regression class as the working-status case", txStore.updatedTask)
	}
}

// TestApplyAction_AttrsSet_Done_NoTaskTriageRow_Rejected pins the doc's
// stated invariant verbatim: "task_triage 行を持たない done の通常 task には
// 発火しない". A task whose task_triage row was never created (the ordinary
// path: pending→executing→done never touches attrs_set at all, so it would
// never actually reach this branch in production — this test exercises the
// GUARD directly, regardless of how such a task could arrive here) must be
// rejected exactly like sm.Apply's existing "no transition" error, not
// silently admitted.
func TestApplyAction_AttrsSet_Done_NoTaskTriageRow_Rejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task} // triage map left empty: no row for t1
	svc := newDoneTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"observed":{"source_closed":false}}`),
	})
	if err == nil {
		t.Fatal("ApplyAction(attrs_set on done, no task_triage row) succeeded, want rejection")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if statusErr.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409 (matches sm.Apply's ordinary 'no transition' rejection)", statusErr.Code)
	}
}

// TestApplyAction_AttrsSet_Done_TaskTriageStoreNotWired_Rejected: when
// s.TaskTriage itself is nil (construction gap), the guard must take the
// SAME safe direction as resolveReopenVariant's own nil-store branch — an
// indeterminate answer must not accidentally ADMIT the write. Uses the
// ordinary (unmodified) newTriageWorkflowService, which deliberately leaves
// TaskTriage nil.
func TestApplyAction_AttrsSet_Done_TaskTriageStoreNotWired_Rejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}, // a row DOES exist in the store...
	}
	svc := newTriageWorkflowService(task, txStore) // ...but TaskTriage is left nil, so the guard can't see it.

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"observed":{"source_closed":false}}`),
	})
	if err == nil {
		t.Fatal("ApplyAction(attrs_set on done, TaskTriage store not wired) succeeded, want rejection")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusConflict {
		t.Fatalf("error = %v, want *StatusError{Code: 409}", err)
	}
}

// TestApplyAction_AttrsSet_Done_TaskTriageLookupError_Returns500 pins that a
// GENUINE lookup failure (not sql.ErrNoRows) is surfaced as a real error
// rather than silently reinterpreted as "no row, reject" — the same
// ErrNoRows-vs-other split statusErrorForGetTaskErr and
// applyAttrsSetSideEffect already use elsewhere in this file.
func TestApplyAction_AttrsSet_Done_TaskTriageLookupError_Returns500(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, getTaskTriageErr: errors.New("db unavailable")}
	svc := newDoneTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"observed":{"source_closed":false}}`),
	})
	if err == nil {
		t.Fatal("ApplyAction(attrs_set) succeeded despite a task_triage lookup failure")
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if statusErr.Code != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want 500 (a genuine lookup failure must not be silently treated as rejection)", statusErr.Code)
	}
}

// TestApplyAction_AttrsSet_Working_StillNonTransitioning_NoRegression is a
// sanity check that refactoring ApplyAction's sm.Apply call (to add the
// done-status special case above) did not change behavior for the ORDINARY
// preExecutionStatuses attrs_set path — apply_action_pr4_test.go already
// pins this in more detail; this is a narrow smoke test at the same call
// site the refactor touched.
func TestApplyAction_AttrsSet_Working_StillNonTransitioning_NoRegression(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newDoneTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"now"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(attrs_set on working): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want unchanged (working)", result.Task.Status)
	}
}
