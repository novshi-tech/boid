package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md) ----
//
// These tests pin the deterministic daemon-side steps of the S1/S2 検証
// シナリオ (doc line ~1001). Steps explicitly marked "khi 側" or "LLM 判断"
// in the doc are NOT testable here (they require the khi agent and its
// prompt-driven judgment) — this file only covers what the daemon itself
// does once khi has sent the corresponding action/create request.

// TestApplyAction_AttrsSet_UpdatesDetailWithoutTransitionOrPayloadPollution
// pins S1 step 3: attrs_set updates task_triage.detail, task.Status stays
// unchanged (non-transition). card-model-cleanup PR-2: the original
// "task.Payload NOT polluted (論点6-2)" half of this test is gone — a Card
// task has no Payload field at all anymore, so that pollution is now
// structurally impossible rather than merely unobserved (see
// apply_action_pr3_noted_answered_test.go's
// TestApplyAction_Noted_DoesNotWriteTaskRowOrPollutePayload for the fuller
// explanation of the same substitution).
func TestApplyAction_AttrsSet_UpdatesDetailWithoutTransitionOrPayloadPollution(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"now","summary":"PR comment landed"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want unchanged (working)", result.Task.Status)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected task_triage row to exist after attrs_set")
	}
	children, err := orchestrator.DetailChildren(tt.Detail) // sanity: no children key expected yet
	if err != nil || len(children) != 0 {
		t.Fatalf("unexpected children in detail: %+v err=%v", children, err)
	}
}

// TestApplyAction_AttrsSet_DoesNotWriteTaskRow pins the codex review Major
// fix: attrs_set/child_added/child_specced must never call tx.UpdateTask —
// they are non-transitioning and their payload is fully consumed by the
// side-effect, so writing the task row (built from a pre-Tx snapshot) risks
// silently stomping a concurrently-committed real transition back to the
// stale status the non-transitioning action happened to read.
func TestApplyAction_AttrsSet_DoesNotWriteTaskRow(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"now"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if txStore.updatedTask != nil {
		t.Fatalf("attrs_set called tx.UpdateTask (updatedTask=%+v), want no task-row write", txStore.updatedTask)
	}
	// The sidecar write must still have happened.
	if txStore.triage["t1"] == nil {
		t.Fatal("expected task_triage row to exist after attrs_set even though the task row was not written")
	}
}

// TestApplyAction_AttrsSet_NoTaskTriageRow_LazilyCreatesRow pins
// applyAttrsSetSideEffect's own GetTaskTriage-miss tolerance (workflow_card.go):
// a card whose sidecar lookup reports sql.ErrNoRows must still accept
// attrs_set, lazily creating the row, rather than failing. Deliberately does
// NOT use newTriageWorkflowService (unlike the test above) — that helper
// auto-seeds an empty CardAttrs row for every parked/working fixture, which
// would silently skip the exact GetTaskTriage-miss branch this test exists
// to exercise.
//
// card-model-cleanup PR-2 (docs/plans/card-model-cleanup.md §3.6): this is
// what remains of PR #986 review's Blocker 1 end-to-end regression test
// (formerly TestApplyAction_AttrsSet_NoTaskTriageRow_PreExecutionStatus_
// LazilyCreatesRow in machine_select_test.go) once machineFor's own removed
// sidecar-row lookup/fallback is factored out of the picture — machineFor is
// now a pure function of task.Type (set directly on this fixture, so
// NewCardMachine is picked unconditionally, no lookup involved). What
// remains real and worth pinning is applyAttrsSetSideEffect's own
// miss-tolerant behavior, which is unchanged Go code.
func TestApplyAction_AttrsSet_NoTaskTriageRow_LazilyCreatesRow(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	// txStore.triage is deliberately left nil/empty: no row exists yet.
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
		t.Fatalf("ApplyAction(attrs_set) on a rowless parked task: %v", err)
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

// TestApplyAction_Reopen_NoTaskTriageRow_DroppedStatus_RestoresToParked pins
// recordAndStripSuggestionIfPresent's own GetTaskTriage-miss tolerance
// (workflow_card.go): a direct card-lifecycle transition (reopen:
// dropped→parked) must still succeed against a card whose sidecar lookup
// reports no row — the same ErrNoRows no-op recordAndStripSuggestionIfPresent
// falls back to for attrs_set's sibling side-effect function. Deliberately
// does NOT use newTriageWorkflowService, for the same reason the attrs_set
// test above avoids it (auto-seeding would skip the branch under test).
//
// card-model-cleanup PR-2: what remains of PR #986 follow-up review's
// "dropped missing from isCardLifecycleStatus's fallback set" regression
// test (formerly TestApplyAction_Reopen_NoTaskTriageRow_DroppedStatus_
// RestoresToTriaged in machine_select_test.go) once machineFor's own removed
// status-based fallback is factored out — machineFor no longer needs a
// fallback at all (task.Type alone decides), and card machine v2's own
// `reopen: dropped→parked` edge (NewCardMachine's doc comment) is what this
// test now pins end to end, together with recordAndStripSuggestionIfPresent's
// row-miss tolerance.
func TestApplyAction_Reopen_NoTaskTriageRow_DroppedStatus_RestoresToParked(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusDropped, Card: &orchestrator.CardAttrs{}}
	// txStore.triage is deliberately left nil/empty: no row exists.
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
		t.Fatalf("action type = %q, want reopen", result.Action.Type)
	}
}

// TestApplyAction_AttrsSet_RejectsWhenConcurrentTransitionRacedAhead pins the
// codex review round 2 Major fix: attrs_set/child_added/child_specced
// re-validate against a FRESH in-Tx read of the task, not the pre-Tx
// snapshot. Here the pre-Tx read sees "parked" (still eligible for
// attrs_set), but a concurrent transition has already committed "aborted" by
// the time this Tx opens — a deliberately synthetic race target purely to
// exercise the in-Tx re-validation mechanism, not a claim that anything
// actually transitions a card INTO aborted (aborted is an execution-only
// status; card-model-cleanup PR-2 removed captured/triaged/ready as Go
// identifiers entirely, migration 0045's CHECK constraint would reject this
// exact row shape in the real DB, but this fake store applies no such
// constraint, so it still works to exercise machine-level rule mismatch).
// "aborted" is NOT in attrs_set's FromStatus enumeration ({parked, working,
// dropped} — machine_card.go — plus a "done" special case this test does not
// want to hit, see below), so the action must be rejected (409), and neither
// the action row nor the task_triage side-effect may be recorded.
//
// "dropped" no longer works as this test's race target (PR #987 review round
// 2, BLOCKER N1): attrs_set's FromStatus set grew to include "dropped" (khi
// must be able to attrs_set a "reopen" suggestion onto a dropped card), so a
// race landing on "dropped" would now legitimately succeed instead of being
// rejected — this test's whole point is a race landing somewhere illegal.
// "done" ALSO no longer works: resolveAttrsSetDoneTransition's service-layer
// guard (attrs_set_done.go) admits attrs_set against a done task whenever a
// task_triage row exists — with this test's seedTriage row present, a
// raced-to-"done" status would legitimately no-op-succeed instead of being
// rejected.
func TestApplyAction_AttrsSet_RejectsWhenConcurrentTransitionRacedAhead(t *testing.T) {
	staleTask := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	racedTask := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusAborted, Card: &orchestrator.CardAttrs{}}
	// seedTriage is a pre-existing task_triage row (PR-B: machineFor needs
	// one to pick NewCardMachine for "t1" — without it attrs_set would 400
	// ("not available") before ever reaching the concurrency-race logic this
	// test actually pins). Its Kind is a sentinel this test checks is still
	// present, byte-for-byte, afterward — proof that the rejected attrs_set
	// never called UpsertTaskTriage — since the row's mere existence can no
	// longer be the "no side effect happened" signal once PR-B requires one
	// to exist up front for machineFor's own sake.
	seedTriage := &orchestrator.CardAttrs{TaskID: "t1", Kind: "unchanged-sentinel"}
	txStore := &recordingTxStore{
		task:  racedTask, // what the in-Tx GetTask sees (post-race)
		tasks: map[string]*orchestrator.Task{"t1": racedTask},
		triage: map[string]*orchestrator.CardAttrs{
			"t1": seedTriage,
		},
	}
	svc := &TaskWorkflowService{
		Tasks:      &stubTaskStore{task: staleTask}, // what the pre-Tx read sees (stale)
		Tx:         recordingTransactor{store: txStore},
		Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		TaskTriage: txStore,
	}

	_, err := svc.ApplyAction(context.Background(), "t1", ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"now"}`),
	})
	if err == nil {
		t.Fatal("expected rejection when a concurrent transition raced the task out of attrs_set's allowed FromStatus set")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if len(txStore.actions) != 0 {
		t.Fatalf("expected no action recorded, got %+v", txStore.actions)
	}
	if txStore.triage["t1"] != seedTriage || txStore.triage["t1"].Kind != "unchanged-sentinel" {
		t.Fatalf("expected the pre-existing task_triage row untouched (no side-effect recorded), got %+v", txStore.triage["t1"])
	}
}

// TestApplyAction_AttrsSet_NotAvailableAction pins S1 step 5: attrs_set must
// not appear in AvailableActions for the status it fired from (論点6-1).
//
// card-model-cleanup PR-2: captured/triaged/ready are gone as Go identifiers
// (folded into parked well before this PR — see model.go's TaskStatus doc
// comment); this loop now covers exactly the four statuses a card can
// actually hold (parked/working/done/dropped — migration 0045's CHECK
// constraint), which is a superset of the original list's real intent.
func TestApplyAction_AttrsSet_NotAvailableAction(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusDropped,
	} {
		for _, a := range sm.AvailableActions(status) {
			if a == "attrs_set" || a == "child_added" || a == "child_specced" {
				t.Fatalf("AvailableActions(%s) unexpectedly contains %q", status, a)
			}
		}
	}
}

// TestApplyAction_ChildAddedThenChildSpecced pins S2 step 2: detail.children
// goes open → specced across the two actions.
func TestApplyAction_ChildAddedThenChildSpecced(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_added",
		Payload: []byte(`{"id":"c1","title":"address review comment"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(child_added): %v", err)
	}
	children, err := orchestrator.DetailChildren(txStore.triage["t1"].Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusOpen {
		t.Fatalf("children after child_added = %+v, want single open child", children)
	}

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_specced",
		Payload: []byte(`{"id":"c1","project":"proj-2","behavior":"executor","description":"background for the Web UI","instruction":"fix the review comment"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(child_specced): %v", err)
	}
	children, err = orchestrator.DetailChildren(txStore.triage["t1"].Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusSpecced {
		t.Fatalf("children after child_specced = %+v, want single specced child", children)
	}
	if children[0].Spec == nil || children[0].Spec.Project != "proj-2" {
		t.Fatalf("spec not set correctly: %+v", children[0].Spec)
	}
	// description must round-trip through the child_specced action payload
	// (Web UI-visible field, kept separate from Instruction — nose 2026-08-14
	// feedback: instruction was carrying all the context, leaving the Web UI
	// description empty).
	if children[0].Spec.Description != "background for the Web UI" {
		t.Fatalf("spec.Description = %q, want it to round-trip from the child_specced payload", children[0].Spec.Description)
	}

	// task row must never be written by either action (論点6-2 payload
	// isolation, hardened further by the codex review Major fix that skips
	// tx.UpdateTask entirely for non-transitioning triage actions — see
	// TestApplyAction_AttrsSet_DoesNotWriteTaskRow for the dedicated test).
	if txStore.updatedTask != nil {
		t.Fatalf("child_added/child_specced wrote the task row (updatedTask=%+v), want no write", txStore.updatedTask)
	}
}

func TestApplyAction_ChildSpecced_UnknownChildRejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_specced",
		Payload: []byte(`{"id":"missing","project":"proj-2"}`),
	})
	if err == nil {
		t.Fatal("expected error specc'ing an unknown child id")
	}
}

func TestApplyAction_AttrsSet_MalformedPayloadRejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "attrs_set", Payload: []byte(`[1,2,3]`)})
	if err == nil {
		t.Fatal("expected error for non-object attrs_set payload")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 StatusError, got %v", err)
	}
}

// TestApplyAction_AttrsSet_RejectsEmptyObjectAndNull pins the codex review
// Minor fix: both {} and null must be rejected, matching the documented
// non-empty-object contract (a no-op attrs_set is a caller bug, not silently
// accepted).
func TestApplyAction_AttrsSet_RejectsEmptyObjectAndNull(t *testing.T) {
	for _, payload := range [][]byte{[]byte(`{}`), []byte(`null`)} {
		task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
		svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "attrs_set", Payload: payload})
		if err == nil {
			t.Fatalf("payload %s: expected error for empty attrs_set payload", payload)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("payload %s: expected 400 StatusError, got %v", payload, err)
		}
	}
}

// ---- card 機械 v2: working からの出口2本 (park/done) ----
//
// v1 had three working exits (ready/triage/park, 論点8); card machine v2
// (docs/plans/suggestion-as-state-transition-impl.md §3) drops ready/triage
// entirely (no such statuses exist any more) and adds "done" as working's
// OTHER exit — so v2's working has exactly two, not three.

func TestApplyAction_Working_TwoExits(t *testing.T) {
	cases := []struct {
		action string
		want   orchestrator.TaskStatus
	}{
		{"park", orchestrator.TaskStatusParked},
		{"done", orchestrator.TaskStatusDone},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
			svc := newTriageWorkflowService(task, &recordingTxStore{task: task})
			ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
			result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: c.action})
			if err != nil {
				t.Fatalf("ApplyAction(%s) from working: %v", c.action, err)
			}
			if result.Task.Status != c.want {
				t.Fatalf("status = %q, want %q", result.Task.Status, c.want)
			}
		})
	}
}

// TestApplyAction_Working_Park_DoesNotPollutePayload is the extra "ついでに
// 確認する" check 論点6-2 calls out: park's own payload (wake_at/
// wake_task_id) must ALSO be excluded from task.Payload, not just the three
// new triage-vocabulary actions.
//
// card-model-cleanup PR-2: the original direct "task.Payload untouched"
// assertion is gone — a Card task has no Payload field at all anymore, so
// that pollution is now structurally impossible (see
// apply_action_pr3_noted_answered_test.go's
// TestApplyAction_Noted_DoesNotWriteTaskRowOrPollutePayload for the fuller
// explanation of the same substitution). The WakeTaskID assertion below is
// what actually still proves park's payload landed in the right place.
func TestApplyAction_Working_Park_DoesNotPollutePayload(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
	_, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{
		Type:    "park",
		Payload: []byte(`{"wake_task_id":"child-1"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	if txStore.triage["t1"].WakeTaskID != "child-1" {
		t.Fatalf("WakeTaskID = %q, want child-1", txStore.triage["t1"].WakeTaskID)
	}
}

// TestApplyAction_Park_WithRealCoordinator_DoesNotPanicDispatchGoroutine pins
// the wiring seam found in card-model-cleanup PR-2 review: ApplyAction's
// post-commit dispatch goroutine used to be gated on `s.Coordinator != nil`
// alone, not also `newTask.Exec != nil` (unlike the hook-preview block four
// lines below it, which already had both). Every OTHER ApplyAction test in
// this package either leaves Coordinator nil (see newTriageWorkflowService's
// own doc comment: "pre-execution transitions have no hooks to dispatch") or
// wires a hand-rolled fake that never reaches the real
// orchestrator.Coordinator.DispatchAndAdvance body — production always wires
// a real *orchestrator.Coordinator (wire.go), so none of those tests could
// have caught a card task reaching DispatchAndAdvance's unguarded
// task.Exec.Payload dereference. This test wires the real type instead.
//
// "park" (working -> parked) is one of the many card manual actions NOT
// redirected to acceptGo/applyAnswered earlier in ApplyAction, so it reaches
// the gated goroutine launch just like drop/done/reopen/attrs_set/child_*/
// noted do.
//
// Before the fix, this test panicked the whole test binary (an unrecovered
// goroutine panic), not merely failed an assertion — Shutdown() blocking
// until the background goroutine returns is what makes that panic surface
// synchronously within this test instead of racing process exit.
func TestApplyAction_Park_WithRealCoordinator_DoesNotPanicDispatchGoroutine(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)
	svc.Coordinator = &orchestrator.Coordinator{Evaluator: &orchestrator.Evaluator{}}

	_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park"})
	if err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	svc.Shutdown()
}

// ---- 論点9: child_dispatched/child_closed cannot be pushed externally ----

func TestApplyAction_ChildDispatchedAndChildClosed_RejectedWhenPushed(t *testing.T) {
	for _, actionType := range []string{"child_dispatched", "child_closed"} {
		task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
		svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: actionType})
		if err == nil {
			t.Fatalf("action %q: expected rejection via public ApplyAction (daemon self-record only), got nil error", actionType)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("action %q: expected 400 StatusError, got %v", actionType, err)
		}
	}
}

// TestFinalizeTerminal_ChildClosed_SelfRecordsOnParent pins S2 step 4: when a
// dispatched child task reaches done, the daemon (not khi) marks it closed on
// the parent's task_triage.detail and appends a child_closed action to the
// PARENT's action log — with no khi involvement (finalizeTerminal is called
// from the child's own terminal-transition paths, never from an
// externally-reachable op).
func TestFinalizeTerminal_ChildClosed_SelfRecordsOnParent(t *testing.T) {
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

	updatedDetail := txStore.triage["parent-1"].Detail
	children, err := orchestrator.DetailChildren(updatedDetail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children = %+v, want single closed child", children)
	}

	found := false
	for _, a := range txStore.actions {
		if a.Type == "child_closed" && a.TaskID == "parent-1" {
			found = true
			if a.Actor != orchestrator.ActorDaemon {
				t.Errorf("child_closed actor = %q, want %q (this is the daemon's own self-record, not a human/task-triggered write)", a.Actor, orchestrator.ActorDaemon)
			}
		}
	}
	if !found {
		t.Fatalf("expected a child_closed action recorded against the parent, got actions=%+v", txStore.actions)
	}

	// Idempotent: calling finalizeTerminal again (e.g. CompleteJob racing a
	// retry) must not append a second child_closed action.
	countBefore := len(txStore.actions)
	svc.finalizeTerminal(context.Background(), child)
	if len(txStore.actions) != countBefore {
		t.Fatalf("finalizeTerminal called twice recorded %d new actions, want 0 (must be idempotent)", len(txStore.actions)-countBefore)
	}
}

// TestFinalizeTerminal_NonChildTask_NoOp confirms a terminal task with no
// ParentID (an ordinary top-level task, or a triage card itself never has a
// parent) does not attempt any parent lookup.
func TestFinalizeTerminal_NonChildTask_NoOp(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeExecution, ProjectID: "p1", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{Tx: recordingTransactor{store: txStore}}

	svc.finalizeTerminal(context.Background(), task)

	if len(txStore.actions) != 0 {
		t.Fatalf("expected no actions recorded, got %+v", txStore.actions)
	}
}

// ---- child_dropped (khi による子の取り下げ) ------------------------------

// TestApplyAction_ChildDropped_ClosesSpeccedChild pins the gap child_closed
// structurally cannot fill: that one matches on TaskRef, which a child only
// gets once it is task-ified, so an unwanted open/specced child had no route
// to "closed" — and ShouldAutoDone requires EVERY child closed.
func TestApplyAction_ChildDropped_ClosesSpeccedChild(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	for _, req := range []ApplyActionRequest{
		{Type: "child_added", Payload: []byte(`{"id":"c1","title":"wrong project baked in"}`)},
		{Type: "child_specced", Payload: []byte(`{"id":"c1","project":"proj-2"}`)},
	} {
		if _, err := svc.ApplyAction(context.Background(), task.ID, req); err != nil {
			t.Fatalf("ApplyAction(%s): %v", req.Type, err)
		}
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_dropped",
		Payload: []byte(`{"id":"c1","reason":"project が khi 自身を指していた"}`),
	})
	if err != nil {
		t.Fatalf("ApplyAction(child_dropped): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want unchanged (working) — child_dropped is non-transitioning", result.Task.Status)
	}
	// card-model-cleanup PR-2: the original "task.Payload untouched" check
	// here (proving "the reason belongs in the action row only") is gone — a
	// Card task has no Payload field at all anymore, so that pollution is now
	// structurally impossible (see apply_action_pr3_noted_answered_test.go's
	// TestApplyAction_Noted_DoesNotWriteTaskRowOrPollutePayload for the fuller
	// explanation of the same substitution). The tx.UpdateTask check below is
	// what actually still proves child_dropped never touched the task row.
	if txStore.updatedTask != nil {
		t.Fatalf("child_dropped wrote the task row (updatedTask=%+v), want no write", txStore.updatedTask)
	}
	children, err := orchestrator.DetailChildren(txStore.triage["t1"].Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	if len(children) != 1 || children[0].Status != orchestrator.TaskTriageChildStatusClosed {
		t.Fatalf("children after child_dropped = %+v, want single closed child", children)
	}
}

// TestApplyAction_ChildDropped_RefusesDispatched keeps khi from closing work
// that is actually running: ShouldAutoDone would then fire while the child's
// own task is still in flight.
func TestApplyAction_ChildDropped_RefusesDispatched(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "child-task",
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	txStore.triage = map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1", Detail: detail}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_dropped",
		Payload: []byte(`{"id":"c1"}`),
	}); err == nil {
		t.Fatal("expected an error dropping a dispatched child")
	}
}

func TestApplyAction_ChildDropped_UnknownChildRejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_dropped",
		Payload: []byte(`{"id":"missing"}`),
	}); err == nil {
		t.Fatal("expected error dropping an unknown child id")
	}
}

func TestApplyAction_ChildDropped_MissingIDRejected(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "child_dropped",
		Payload: []byte(`{"reason":"id が無い"}`),
	}); err == nil {
		t.Fatal("expected 400 for a child_dropped payload without id")
	}
}
