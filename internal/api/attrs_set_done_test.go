package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of the test, restoring the previous default on cleanup.
// Mirrors internal/config/config_test.go's own captureSlog helper — no
// existing test in this package captured slog output before N-1's fix, so
// this is a parallel (not shared) helper local to this test's need.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

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
// Most of these tests do not assert on the I-5c log line itself (this
// codebase's existing sweep tests — queue_sweep_test.go — likewise never
// assert on slog output, only on returned/stored state): I-5c's visibility
// is pinned structurally here by confirming the write actually lands (the
// log call is unconditionally reached on that same success path).
//
// TestLogAttrsSetOnDoneTriage_LogsAtWarnLevel below is the one exception
// (2026-08-19, Opus review N-1): the "write lands" pin above cannot detect
// deleting the log call itself (workflow_action.go's own `if req.Type ==
// "attrs_set" && ...` block around logAttrsSetOnDoneTriage) — the sidecar
// fold and the log line are two independent statements on the same success
// path, and only one of them is exercised by the state-based assertions
// here. That test captures slog output directly to close the gap.

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
// rejected, not silently admitted.
//
// PR-B behavior change (docs/plans/suggestion-as-state-transition-impl.md
// §2): the expected status code moved from 409 to 400. Before the machine
// split, "done" always went to the one unified machine (attrs_set IS
// Manual:true somewhere in it, for the preExecutionStatuses statuses), so
// the request passed ApplyAction's IsManualAction gate and only failed
// later, inside resolveAttrsSetDoneTransition's own guard, as a 409
// ("no transition"). Now machineFor performs THE SAME task_triage lookup
// (no row = not a card) even earlier than that, right after the task loads
// — and routes a rowless "done" task to NewExecutionMachine, which has no
// "attrs_set" rule AT ALL (attrs_set is card-only). So the request is
// rejected at the IsManualAction gate itself, as a 400, before
// resolveAttrsSetDoneTransition's own guard is ever reached. The invariant
// itself ("no row ⇒ attrs_set does not fire") is unchanged; only the code
// and the code path that enforces it are.
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
	if statusErr.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400 (PR-B: machineFor's own task_triage lookup now rejects this before resolveAttrsSetDoneTransition's guard is ever reached — see this test's own doc comment)", statusErr.Code)
	}
}

// TestApplyAction_AttrsSet_Done_TaskTriageStoreNotWired_Rejected: when
// s.TaskTriage itself is nil (construction gap), the guard must take the
// SAME safe direction as resolveReopenVariant's own nil-store branch — an
// indeterminate answer must not accidentally ADMIT the write. Uses the
// ordinary (unmodified) newTriageWorkflowService, which deliberately leaves
// TaskTriage nil.
//
// PR-B behavior change: same reasoning as
// TestApplyAction_AttrsSet_Done_NoTaskTriageRow_Rejected above — machineFor
// ALSO reads s.TaskTriage (the same nil field), and machineFor's OWN nil-store
// handling falls back to NewExecutionMachine (a softer default than
// resolveAttrsSetDoneTransition's — see machineFor's own doc comment for why
// that asymmetry is safe), so the request is rejected earlier, as a 400, not
// a 409.
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
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("error = %v, want *StatusError{Code: 400} (PR-B: machineFor's nil-store fallback rejects this earlier than resolveAttrsSetDoneTransition's own guard — see this test's own doc comment)", err)
	}
}

// TestApplyAction_AttrsSet_Done_TaskTriageLookupError_Returns503 pins that a
// GENUINE lookup failure (not sql.ErrNoRows) is surfaced as a real error
// rather than silently reinterpreted as "no row, reject" — the same
// ErrNoRows-vs-other split statusErrorForGetTaskErr and
// applyAttrsSetSideEffect already use elsewhere in this file.
//
// PR-B behavior change: the expected status code moved from 500 to 503, and
// the failure now originates in machineFor rather than
// resolveAttrsSetDoneTransition — machineFor performs the identical
// s.TaskTriage.GetTaskTriage lookup EARLIER (right after the task loads),
// hits the same transient error first, and reports it the way
// resolveReopenVariant already does for an indeterminate sidecar lookup
// (503, not 500) — see machineFor's own doc comment.
func TestApplyAction_AttrsSet_Done_TaskTriageLookupError_Returns503(t *testing.T) {
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
	if statusErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want 503 (a genuine lookup failure must not be silently treated as rejection; PR-B moved the failing lookup into machineFor — see this test's own doc comment)", statusErr.Code)
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

// TestLogAttrsSetOnDoneTriage_LogsAtWarnLevel pins two things N-1 (Opus
// review, 2026-08-19) found missing: (1) the log level is Warn, not Info —
// matching the precedent this code's own doc comment cites
// (logCanonicalSourceBreaches, queue_sweep.go, which logs its real breach
// case at Warn) and mattering concretely because `log.level: warn`
// (config-yaml.md) silently drops an Info line while every other
// 決定16-class signal in this codebase stays visible; (2) the log call
// itself actually fires on this path — deleting workflow_action.go's `if
// req.Type == "attrs_set" && ...` block around logAttrsSetOnDoneTriage
// entirely would not be caught by any of the state-based assertions above
// (the sidecar fold and the log line are independent statements on the
// same success path), so this test captures slog output directly instead.
func TestLogAttrsSetOnDoneTriage_LogsAtWarnLevel(t *testing.T) {
	buf := captureSlog(t)

	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:   task,
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: []byte(`{"attrs":{"observed":{"source_closed":true}}}`)}},
	}
	svc := newDoneTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"summary":"still closed, another update arrived"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set on done triage task): %v", err)
	}

	out := buf.String()
	var logLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "I-5c") {
			logLine = line
			break
		}
	}
	if logLine == "" {
		t.Fatalf("no I-5c log line found; full output:\n%s", out)
	}
	if !strings.Contains(logLine, "level=WARN") {
		t.Errorf("I-5c log line = %q, want level=WARN (not Info — it must survive log.level: warn)", logLine)
	}
}

// TestApplyAction_AttrsSet_LogsUsingInTxStatus_NotStalePreTxSnapshot pins
// N-5 (Opus review, 2026-08-19): workflow_action.go's I-5c log condition
// must key on action.FromStatus (the IN-TX re-validated value) rather than
// the pre-Tx local `fromStatus` snapshot.
//
// Race modeled here: ApplyAction's pre-Tx read (s.Tasks.GetTask, via the
// separate stubTaskStore below) sees the task still "working" — a
// concurrent "done" (a different actor's accept(done), or a direct click)
// committed working→done in the gap before this Tx opens. Since attrs_set
// is in skipTaskUpdate, the code re-validates from a FRESH in-Tx read
// (tx.GetTask, via txStore below, deliberately primed to "done" to model
// that race) — I-5b's guard is what admits THIS attrs_set, and
// action.FromStatus gets overwritten to "done" accordingly. The log must
// fire based on THAT fresh value, not the stale pre-Tx "working" the old
// `fromStatus`-keyed condition would have checked (which would have
// wrongly skipped the log here).
func TestApplyAction_AttrsSet_LogsUsingInTxStatus_NotStalePreTxSnapshot(t *testing.T) {
	buf := captureSlog(t)

	preTx := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	fresh := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{
		task:   fresh, // tx.GetTask (in-Tx) sees the task as already done
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: []byte(`{"attrs":{"observed":{"source_closed":true}}}`)}},
	}
	svc := newDoneTriageWorkflowService(preTx, txStore) // s.Tasks (pre-Tx) sees "working"

	if _, err := svc.ApplyAction(context.Background(), "t1", ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"summary":"raced with a concurrent done"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set, racing done): %v", err)
	}

	if !strings.Contains(buf.String(), "I-5c") {
		t.Fatalf("expected an I-5c log line (action.FromStatus was re-validated to done in-Tx); got:\n%s", buf.String())
	}
}
