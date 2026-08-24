package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestMachineFor_SidecarRowExists_PicksCardMachine pins machineFor's core
// discriminator (PR-B, docs/plans/suggestion-as-state-transition-impl.md
// §2): a task carrying a task_triage sidecar row is a card, so machineFor
// must return a machine that knows card-vocabulary rules (go/park/drop/etc.)
// and does NOT know execution-only rules (start/ask/answer/abort).
func TestMachineFor_SidecarRowExists_PicksCardMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusParked}
	store := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}

	sm, err := machineFor(store, task)
	if err != nil {
		t.Fatalf("machineFor: %v", err)
	}
	if !sm.IsManualAction("go") {
		t.Error("expected the card machine (IsManualAction(\"go\") = true)")
	}
	if sm.IsManualAction("start") || sm.IsManualAction("abort") {
		t.Error("expected the card machine to NOT know execution-only actions (start/abort)")
	}
}

// TestMachineFor_NoSidecarRow_PicksExecutionMachine is the mirror: an
// ordinary task (no task_triage row — sql.ErrNoRows) gets the execution
// machine.
func TestMachineFor_NoSidecarRow_PicksExecutionMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusExecuting}
	store := &stubTriageStore{} // empty rows: GetTaskTriage returns errNoTriageRow

	sm, err := machineFor(store, task)
	if err != nil {
		t.Fatalf("machineFor: %v", err)
	}
	if !sm.IsManualAction("start") || !sm.IsManualAction("abort") {
		t.Error("expected the execution machine (IsManualAction(\"start\"/\"abort\") = true)")
	}
	if sm.IsManualAction("triage") || sm.IsManualAction("park") {
		t.Error("expected the execution machine to NOT know card-only actions (triage/park)")
	}
}

// TestMachineFor_NilStore_FallsBackToExecutionMachine pins machineFor's own
// documented nil-store fallback (see its doc comment, machine_select.go):
// unlike resolveReopenVariant (which 503s on a nil store, the dangerous
// branch), machineFor falls back to NewExecutionMachine — the overwhelmingly
// common case and the one every pre-PR-B caller effectively assumed.
func TestMachineFor_NilStore_FallsBackToExecutionMachine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusPending}

	sm, err := machineFor(nil, task)
	if err != nil {
		t.Fatalf("machineFor(nil store): %v", err)
	}
	if !sm.IsManualAction("start") {
		t.Error("expected the execution machine for a nil TaskTriageStore")
	}
}

// TestMachineFor_LookupError_ReturnsServiceUnavailable pins that a GENUINE
// task_triage lookup failure (not sql.ErrNoRows) is indeterminate and must
// not be silently guessed either way — same posture as
// resolveReopenVariant's own equivalent branch (triage_done.go).
func TestMachineFor_LookupError_ReturnsServiceUnavailable(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusDone}
	store := &stubTriageStore{getErr: errors.New("db unavailable")}

	sm, err := machineFor(store, task)
	if sm != nil {
		t.Errorf("expected a nil machine on lookup failure, got %+v", sm)
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *StatusError", err)
	}
	if se.Code != http.StatusServiceUnavailable {
		t.Errorf("status code = %d, want %d", se.Code, http.StatusServiceUnavailable)
	}
}

// TestMachineFor_NoConfirmedRow_UnambiguousCardStatus_FallsBackToCardMachine
// is the regression test for PR #986 review Blocker 1 (and its follow-up
// review round, which caught that "dropped" was missing from the fallback
// set): task_create.go's SeedTaskTriage is deliberately best-effort at
// creation time, so a task can legitimately sit in captured/triaged/parked/
// ready/working/dropped with NO task_triage row yet. Before this fix,
// machineFor collapsed "no confirmed row" straight to NewExecutionMachine,
// which has no rule for ANY of these six statuses or ANY card verb —
// permanently stranding such a task (every card action 400s, delete is the
// only way out). machineFor must instead fall back to NewCardMachine by
// STATUS in this case, covering BOTH ways "no confirmed row" can arise: a
// nil store, and a non-nil store that genuinely has no row (sql.ErrNoRows).
//
// "dropped" belongs in this set for the same reason the other five do (see
// isCardLifecycleStatus's own doc comment): it is unambiguously card-only —
// grepping the rule tables shows every `ToStatus: "dropped"` rule lives in
// NewCardMachine, and task_create.go's allowedCreateInitialStatuses cannot
// create a task directly into "dropped" either. Leaving it out (an earlier
// version of this fix did) reopened Blocker 1's exact gap for a rowless
// dropped card's `reopen`: NewExecutionMachine has no dropped→anything rule,
// so the request 409'd instead of reaching NewCardMachine's
// `reopen: dropped→triaged` "recovery from a mistaken drop" rule.
func TestMachineFor_NoConfirmedRow_UnambiguousCardStatus_FallsBackToCardMachine(t *testing.T) {
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDropped,
	}
	for _, status := range statuses {
		task := &orchestrator.Task{ID: "t1", Status: status}

		t.Run(string(status)+"/nil store", func(t *testing.T) {
			sm, err := machineFor(nil, task)
			if err != nil {
				t.Fatalf("machineFor: %v", err)
			}
			if !sm.IsManualAction("triage") && !sm.IsManualAction("park") && !sm.IsManualAction("drop") && !sm.IsManualAction("attrs_set") {
				t.Errorf("status %s, nil store: expected the card machine, got one with no card verbs", status)
			}
		})

		t.Run(string(status)+"/store present but no row", func(t *testing.T) {
			store := &stubTriageStore{} // empty: GetTaskTriage returns errNoTriageRow
			sm, err := machineFor(store, task)
			if err != nil {
				t.Fatalf("machineFor: %v", err)
			}
			if !sm.IsManualAction("attrs_set") {
				t.Errorf("status %s, rowless store: expected the card machine (attrs_set manual), got the execution machine", status)
			}
		})
	}
}

// TestMachineFor_NoConfirmedRow_DoneOrAborted_StillPicksExecutionMachine is
// the safety-net half of the Blocker 1 fix: done/aborted must NOT get the
// same status-based fallback, because a done card and a done ordinary task
// are indistinguishable by status alone — only a CONFIRMED sidecar row may
// decide for those two statuses (same reasoning resolveReopenVariant already
// applies). This pins that the fix does not overreach into guessing on the
// genuinely ambiguous statuses.
func TestMachineFor_NoConfirmedRow_DoneOrAborted_StillPicksExecutionMachine(t *testing.T) {
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		task := &orchestrator.Task{ID: "t1", Status: status}
		store := &stubTriageStore{} // no row
		sm, err := machineFor(store, task)
		if err != nil {
			t.Fatalf("machineFor: %v", err)
		}
		if !sm.IsManualAction("reopen") || sm.IsManualAction("triage") {
			t.Errorf("status %s: expected the execution machine (unconfirmed row must not fall back by status for done/aborted), got %+v", status, sm)
		}
	}
}

// TestMachineForDisplay_LookupError_FallsBackByStatus is the regression test
// for PR #986 review Blocker 2: a genuine (non-ErrNoRows) task_triage lookup
// failure must not propagate out of a read-only call site — machineForDisplay
// swallows the error and guesses by status instead, same fallback rule
// machineFor's own "no confirmed row" branch uses.
func TestMachineForDisplay_LookupError_FallsBackByStatus(t *testing.T) {
	failingStore := &stubTriageStore{getErr: errors.New("db unavailable")}

	t.Run("pre-execution status falls back to card machine", func(t *testing.T) {
		task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusTriaged}
		sm := machineForDisplay(failingStore, task)
		if sm == nil {
			t.Fatal("machineForDisplay returned nil, want a usable machine")
		}
		if !sm.IsManualAction("attrs_set") {
			t.Error("expected the card machine for a pre-execution-status task")
		}
	})

	t.Run("done status falls back to execution machine", func(t *testing.T) {
		task := &orchestrator.Task{ID: "t1", Status: orchestrator.TaskStatusDone}
		sm := machineForDisplay(failingStore, task)
		if sm == nil {
			t.Fatal("machineForDisplay returned nil, want a usable machine")
		}
		if !sm.IsManualAction("reopen") || sm.IsManualAction("triage") {
			t.Error("expected the execution machine for a done-status task (ambiguous statuses never guess by status)")
		}
	})
}

// TestHasTaskTriageRow_SharedBySelectorAndReopenRouting pins that
// hasTaskTriageRow (the primitive machineFor and resolveReopenVariant both
// now share) reports the correct bool/error split across all three of a
// store's possible answers: row exists, sql.ErrNoRows, and a genuine other
// error.
func TestHasTaskTriageRow_SharedBySelectorAndReopenRouting(t *testing.T) {
	t.Run("row exists", func(t *testing.T) {
		store := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err != nil || !isCard {
			t.Fatalf("isCard=%v err=%v, want true/nil", isCard, err)
		}
	})
	t.Run("no row (ErrNoRows)", func(t *testing.T) {
		store := &stubTriageStore{}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err != nil || isCard {
			t.Fatalf("isCard=%v err=%v, want false/nil", isCard, err)
		}
	})
	t.Run("genuine lookup error", func(t *testing.T) {
		store := &stubTriageStore{getErr: errors.New("db unavailable")}
		isCard, err := hasTaskTriageRow(store, "t1")
		if err == nil || isCard {
			t.Fatalf("isCard=%v err=%v, want false/non-nil", isCard, err)
		}
	})
}

// ---- End-to-end regression tests for PR #986 review's two Blockers ----

// TestApplyAction_AttrsSet_NoTaskTriageRow_PreExecutionStatus_LazilyCreatesRow
// is Blocker 1's full-stack regression: not just that machineFor PICKS the
// card machine for a rowless parked/working task (machine_select_test.go's
// unit tests above), but that a REAL ApplyAction call for a card verb
// against such a task actually SUCCEEDS end to end and lazily creates the
// task_triage row via the side effect (applyAttrsSetSideEffect) — restoring
// the exact recovery path task_create.go's SeedTaskTriage doc comment
// promises when the seed at creation time fails.
func TestApplyAction_AttrsSet_NoTaskTriageRow_PreExecutionStatus_LazilyCreatesRow(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	// txStore.triage is deliberately left nil/empty — simulating a
	// SeedTaskTriage failure at task-creation time (task_create.go logs and
	// continues rather than failing the create).
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: task},
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage: txStore,
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"now"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(attrs_set) on a rowless parked task: %v (Blocker 1: the task must not be permanently stuck)", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want unchanged (parked; attrs_set is non-transitioning)", result.Task.Status)
	}
	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected the task_triage row to be lazily created by applyAttrsSetSideEffect")
	}
	if got.Urgency != "now" {
		t.Fatalf("Urgency = %q, want %q (the side effect must actually have run, not just been permitted)", got.Urgency, "now")
	}
}

// TestApplyAction_Reopen_NoTaskTriageRow_DroppedStatus_RestoresToTriaged is
// the exact reproduction the PR #986 follow-up review gave for "dropped"
// being missing from isCardLifecycleStatus's fallback set: a rowless card
// (SeedTaskTriage failed at creation) gets dropped (now reachable end to end
// per TestApplyAction_AttrsSet_NoTaskTriageRow_PreExecutionStatus_LazilyCreatesRow's
// sibling coverage of "drop" itself), leaving it rowless AND dropped. Before
// this fix, `reopen` on that task fell to NewExecutionMachine (dropped is
// not in isCardLifecycleStatus yet), whose `reopen` rule only covers
// done/aborted→executing — no rule matches FromStatus "dropped", so
// sm.Apply 409'd. NewCardMachine's own `reopen: dropped→parked` rule
// ("recovery from a mistaken drop") was reachable in the pre-split unified
// machine and must stay reachable now — v2 keeps the same edge, just with
// "parked" as the destination instead of v1's "triaged" (card machine v2
// has no "triaged" status at all — docs/plans/
// suggestion-as-state-transition-impl.md §3.2).
func TestApplyAction_Reopen_NoTaskTriageRow_DroppedStatus_RestoresToParked(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDropped, Behavior: "dev", Payload: []byte(`{}`)}
	// txStore.triage is deliberately left nil/empty — the row was never
	// seeded (or was lost) before this card was dropped.
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: task},
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage: txStore,
	}

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "reopen"})
	if err != nil {
		t.Fatalf("ApplyAction(reopen) on a rowless dropped card: %v (the undo-a-mistaken-drop path must not be lost)", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", result.Task.Status)
	}
	if result.Action.Type != "reopen" {
		t.Fatalf("action type = %q, want reopen (card machine's own rule — v2 no longer needs a separate reopen_triaged name, see NewCardMachine's doc comment)", result.Action.Type)
	}
}

// TestTaskAppServiceGetTaskDetail_TaskTriageLookupError_StillSucceeds is
// Blocker 2's regression: TaskAppService.GetTaskDetail is a pure read, so a
// genuine (non-ErrNoRows) task_triage lookup failure must not fail the whole
// call — machineForDisplay's fallback keeps the page rendering.
func TestTaskAppServiceGetTaskDetail_TaskTriageLookupError_StillSucceeds(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev"}
	svc := &TaskAppService{
		Tasks:      &stubTaskStore{task: task},
		Actions:    stubActionStore{},
		Jobs:       &stubJobStore{},
		TaskTriage: &stubTriageStore{getErr: errors.New("db unavailable")},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v, want nil (a transient task_triage lookup failure must not fail a pure read)", err)
	}
	if got.Task.ID != task.ID {
		t.Fatalf("task id = %q, want %q", got.Task.ID, task.ID)
	}
}

// TestWebAppServiceGetTaskDetail_TaskTriageLookupError_StillSucceeds is the
// Web UI's half of the same Blocker 2 regression: before the fix,
// WebHandler.TaskDetail would have rendered this as an opaque "Task not
// found" 404 for what is actually just a transient DB blip.
func TestWebAppServiceGetTaskDetail_TaskTriageLookupError_StillSucceeds(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev"}
	svc := &WebAppService{
		Tasks:      &stubTaskStore{task: task},
		Actions:    stubActionStore{},
		Jobs:       &stubJobStore{},
		TaskTriage: &stubTriageStore{getErr: errors.New("db unavailable")},
	}

	got, err := svc.GetTaskDetail(task.ID)
	if err != nil {
		t.Fatalf("GetTaskDetail() error = %v, want nil (a transient task_triage lookup failure must not fail a pure read)", err)
	}
	if got.Task.ID != task.ID {
		t.Fatalf("task id = %q, want %q", got.Task.ID, task.ID)
	}
}
