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

// sharedKnownActions are the six action names both machine.go doc comments
// say are deliberately duplicated onto BOTH machines (job_failed/progress/
// done_request/fail_request/child_dispatched/child_closed): the real writers
// bypass sm.Apply and call tx.CreateAction directly, so registering them on
// both is only about StateMachine.Apply/IsManualAction treating the name as
// "known" regardless of which machine a caller happens to be holding.
var sharedKnownActions = []string{"job_failed", "progress", "done_request", "fail_request", "child_dispatched", "child_closed"}

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
// requirement).
func TestCardMachine_HasNoExecutionVocabulary(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	executionOnly := []string{"start", "done", "fail", "ask", "answer"}
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

// TestCardMachine_Reopen_DoneAbortedNotHandled is the mirror: NewCardMachine's
// reopen rule only covers dropped→triaged — done/aborted→executing belongs
// exclusively to NewExecutionMachine (a done/aborted CARD is reopened via
// reopen_triaged instead, resolved by api.resolveReopenVariant, not by this
// "reopen" rule).
func TestCardMachine_Reopen_DoneAbortedNotHandled(t *testing.T) {
	sm := orchestrator.NewCardMachine()
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		task := &orchestrator.Task{Status: status}
		if _, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"}); err == nil {
			t.Errorf("NewCardMachine: reopen from %s unexpectedly succeeded", status)
		}
	}
}
