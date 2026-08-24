package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Generic StateMachine infrastructure (not tied to either
// NewExecutionMachine or NewCardMachine — exercises the shared engine
// directly via a hand-built rule table). See machine_execution_test.go and
// machine_card_test.go for the two concrete machines PR-B split NewMachine
// into. ----

func TestStateMachine_Advance_ConditionMet(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "done",
				Condition: func(payload json.RawMessage) bool {
					var m map[string]json.RawMessage
					_ = json.Unmarshal(payload, &m)
					_, ok := m["artifact"]
					return ok
				},
			},
		},
	}

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"url":"https://github.com/..."}}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected Advance to return ok=true")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestStateMachine_Apply_IgnoresConditionRules(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{
				FromStatus: "executing",
				ToStatus:   "done",
				Condition: func(payload json.RawMessage) bool {
					return true
				},
			},
		},
	}

	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "verify"})
	if err == nil {
		t.Fatal("Apply should not match condition-based rules via action")
	}
}

// ---- PR-B (docs/plans/suggestion-as-state-transition-impl.md §2): pin the
// rule-attribution boundary between NewExecutionMachine and NewCardMachine
// itself, on top of each machine's own behavioral tests
// (machine_execution_test.go / machine_card_test.go). ----

// sharedKnownActions are the five action names both machine.go doc comments
// say are deliberately duplicated onto BOTH machines (progress/done_request/
// fail_request/child_dispatched/child_closed): the real writers bypass
// sm.Apply and call tx.CreateAction directly, so registering them on both is
// only about StateMachine.Apply/IsManualAction treating the name as "known"
// regardless of which machine a caller happens to be holding.
//
// job_failed is NOT in this list as of card machine v2 (docs/plans/
// suggestion-as-state-transition-impl.md §3): it targets "aborted", the one
// status v2's card machine deliberately never reaches again (design doc
// §3.2's "card は aborted にも到達しなくなる... card 機械は4状態で本当に閉じる")
// — see TestCardMachineV2_JobFailed_NotRegistered
// (machine_card_test.go) for the dedicated pin, and
// TestJobFailed_ExecutionOnly below for the negative half.
var sharedKnownActions = []string{"progress", "done_request", "fail_request", "child_dispatched", "child_closed"}

// TestJobFailed_ExecutionOnly pins job_failed's post-v2 asymmetry: registered
// (Manual:false, "known") on NewExecutionMachine only, absent entirely from
// NewCardMachine.
func TestJobFailed_ExecutionOnly(t *testing.T) {
	exec := orchestrator.NewExecutionMachine()
	if exec.IsManualAction("job_failed") {
		t.Error("NewExecutionMachine: IsManualAction(\"job_failed\") = true, want false")
	}
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	if _, err := exec.Apply(task, &orchestrator.Action{Type: "job_failed"}); err != nil {
		t.Errorf("NewExecutionMachine: Apply(job_failed) unexpectedly errored: %v", err)
	}

	card := orchestrator.NewCardMachine()
	if card.IsManualAction("job_failed") {
		t.Error("NewCardMachine: IsManualAction(\"job_failed\") = true, want false")
	}
	cardTask := &orchestrator.Task{Status: orchestrator.TaskStatusWorking}
	if _, err := card.Apply(cardTask, &orchestrator.Action{Type: "job_failed"}); err == nil {
		t.Error("NewCardMachine: Apply(job_failed) unexpectedly succeeded — job_failed must not be a card rule in v2")
	}
}

func TestSharedActions_RegisteredOnBothMachines(t *testing.T) {
	for _, name := range sharedKnownActions {
		for _, mkMachine := range []struct {
			label string
			sm    *orchestrator.StateMachine
		}{
			{"execution", orchestrator.NewExecutionMachine()},
			{"card", orchestrator.NewCardMachine()},
		} {
			// None of the six are Manual:true on either machine — the real
			// writers self-record via tx.CreateAction, never through the
			// public ApplyAction/IsManualAction gate.
			if mkMachine.sm.IsManualAction(name) {
				t.Errorf("%s: IsManualAction(%q) = true, want false (all six shared actions are Manual:false)", mkMachine.label, name)
			}
			// Apply must recognize the name as known (FromStatus "*") rather
			// than erroring "no transition for action", regardless of the
			// task's status.
			task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
			if _, err := mkMachine.sm.Apply(task, &orchestrator.Action{Type: name}); err != nil {
				t.Errorf("%s: Apply(%q) unexpectedly errored: %v", mkMachine.label, name, err)
			}
		}
	}
}

// TestExecutionMachine_HasNoCardVocabulary pins the attribution table's
// execution-machine column by NEGATION: none of the card-only action names
// are Manual:true on NewExecutionMachine (reopen is deliberately excluded —
// both machines have a rule NAMED "reopen", but with disjoint FromStatus
// sets; see TestExecutionMachine_Reopen_DroppedStatusNotHandled and
// TestCardMachine_Reopen_DoneAbortedNotHandled below for the FromStatus-level
// pin instead).
func TestExecutionMachine_HasNoCardVocabulary(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	cardOnly := []string{
		"triage", "ready", "park",
		"wake_triaged", "wake_ready", "wake_working",
		"drop", "dispatch", "triage_done", "reopen_triaged",
		"attrs_set", "child_added", "child_specced", "child_dropped", "noted", "answered",
	}
	for _, name := range cardOnly {
		if sm.IsManualAction(name) {
			t.Errorf("NewExecutionMachine: IsManualAction(%q) = true, want false (card-only action leaked into the execution machine)", name)
		}
	}
}

// TestCardMachine_HasNoExecutionVocabulary is the symmetric negation for the
// card machine (abort is checked separately below since it also needs an
// AvailableActions-level pin per the PR's own "abort は card 機械に入れない"
// requirement). "done"/"reopen" are deliberately EXCLUDED from this list as
// of card machine v2: both are now legitimate card verbs too (working→done,
// done/dropped→parked) — they share a NAME with an execution-only rule
// without sharing its FromStatus/ToStatus, exactly like "reopen" always did
// (see TestCardMachine_Reopen_ExecutionFromStatusNotHandled below for the
// FromStatus-level pin covering both).
func TestCardMachine_HasNoExecutionVocabulary(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	executionOnly := []string{"start", "fail", "ask", "answer"}
	for _, name := range executionOnly {
		if sm.IsManualAction(name) {
			t.Errorf("NewCardMachine: IsManualAction(%q) = true, want false (execution-only action leaked into the card machine)", name)
		}
	}
}

// TestCardMachine_AbortIsAbsent pins the PR's explicit behavior-diff
// requirement ("abort は card 機械に入れない — 現行は done の card にも abort
// ボタンが出ていたはず"): abort must not be Manual, must not be applicable
// from ANY status via sm.Apply, and must never appear in AvailableActions
// for the card machine.
func TestCardMachine_AbortIsAbsent(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	if sm.IsManualAction("abort") {
		t.Fatal("NewCardMachine: IsManualAction(\"abort\") = true, want false")
	}
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusCaptured,
		orchestrator.TaskStatusTriaged,
		orchestrator.TaskStatusParked,
		orchestrator.TaskStatusReady,
		orchestrator.TaskStatusWorking,
		orchestrator.TaskStatusDropped,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	} {
		task := &orchestrator.Task{Status: status}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: "abort"}); err == nil {
			t.Errorf("NewCardMachine: abort from %s unexpectedly succeeded", status)
		}
		for _, a := range sm.AvailableActions(status) {
			if a == "abort" {
				t.Errorf("NewCardMachine: AvailableActions(%s) unexpectedly contains \"abort\"", status)
			}
		}
	}
}

// TestExecutionMachine_Reopen_DroppedStatusNotHandled pins the FromStatus-
// level half of the "reopen exists on both machines but with disjoint
// FromStatus sets" split: NewExecutionMachine's reopen rule only covers
// done/aborted, never dropped (a status that does not even exist in this
// machine's own island).
func TestExecutionMachine_Reopen_DroppedStatusNotHandled(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDropped}
	if _, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"}); err == nil {
		t.Fatal("NewExecutionMachine: reopen from dropped unexpectedly succeeded")
	}
}

// TestCardMachine_Reopen_ExecutionFromStatusNotHandled is the mirror of
// TestExecutionMachine_Reopen_DroppedStatusNotHandled: NewCardMachine's own
// "reopen" rule covers done/dropped→parked (see machine_card_test.go's
// TestCardMachineV2_AllEdges for the positive pin — done→parked is one of
// v2's seven core edges, a deliberate departure from v1 where a done/aborted
// card's reopen routed to the DIFFERENT "reopen_triaged" name via
// api.resolveReopenVariant). "aborted" is NOT a v2 card status at all — see
// TestCardMachineV2_JobFailed_NotRegistered — so reopen from aborted must
// still fail here exactly as it always did.
func TestCardMachine_Reopen_ExecutionFromStatusNotHandled(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAborted}
	if _, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"}); err == nil {
		t.Error("NewCardMachine: reopen from aborted unexpectedly succeeded")
	}
}

// TestStateMachine_CanApplyTransitionAction_IgnoresNonTransitioningRuleForSameAction
// pins CanApplyTransitionAction's own `|| r.ToStatus == ""` filter (review
// LOW 1, fix/unapplicable-suggestion-guard PR): no action on the REAL card
// machine today has both a Manual transitioning rule AND a Manual
// non-transitioning rule under the same name, so that filter's removal would
// not fail any existing test against NewCardMachine() — this hand-built
// machine manufactures exactly that situation so the filter's own behavior
// is pinned independently of whether the real rule table happens to need it
// yet.
//
// "hybrid" has two rules: a transitioning one (parked→working) and a
// non-transitioning one (done, ToStatus=="") — same action name, disjoint
// FromStatus. From "done", CanApplyManualAction correctly says yes (SOME
// Manual rule matches, transitioning or not — its own doc comment), but
// CanApplyTransitionAction must say no: applying "hybrid" from "done" would
// NOT actually flip the task's status (sm.Apply's ToStatus=="" branch leaves
// status unchanged), so a caller asking "would this fire a real transition"
// must get false here even though CanApplyManualAction says true.
func TestStateMachine_CanApplyTransitionAction_IgnoresNonTransitioningRuleForSameAction(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "synthetic-hybrid",
		Rules: []orchestrator.Rule{
			{Action: "hybrid", FromStatus: "parked", ToStatus: "working", Manual: true},
			{Action: "hybrid", FromStatus: "done", Manual: true}, // ToStatus == "": non-transitioning
		},
	}

	if !sm.CanApplyManualAction("hybrid", orchestrator.TaskStatusDone) {
		t.Fatal("CanApplyManualAction(hybrid, done) = false, want true (a Manual rule matches, non-transitioning or not)")
	}
	if sm.CanApplyTransitionAction("hybrid", orchestrator.TaskStatusDone) {
		t.Error("CanApplyTransitionAction(hybrid, done) = true, want false — the only matching rule from done is non-transitioning (ToStatus==\"\"), so applying it would not change status")
	}

	// From "parked" both must agree: the OTHER rule under the same action
	// name is a real transition here.
	if !sm.CanApplyManualAction("hybrid", orchestrator.TaskStatusParked) {
		t.Fatal("CanApplyManualAction(hybrid, parked) = false, want true")
	}
	if !sm.CanApplyTransitionAction("hybrid", orchestrator.TaskStatusParked) {
		t.Error("CanApplyTransitionAction(hybrid, parked) = false, want true — parked->working is a real transitioning rule")
	}
}
