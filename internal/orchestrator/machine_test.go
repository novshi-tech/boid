package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- DefaultMachine: manual transitions ----

func TestDefaultMachine_PendingToExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestDefaultMachine_ExecutingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_VerifyingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusVerifying}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_ReworkingToDone_Manual(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusReworking}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Reopen_DoneToReworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDone}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_InvalidTransition(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err == nil {
		t.Fatal("expected error for invalid transition pending -> done")
	}
	if !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("expected no transition error, got: %v", err)
	}
}

func TestDefaultMachine_Abort_FromAnyState(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
	}
	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "abort"})
		if err != nil {
			t.Fatalf("abort from %s: %v", status, err)
		}
		if next.Status != orchestrator.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}

func TestDefaultMachine_JobFailed_FromAnyState(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
	}
	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		next, err := sm.Apply(task, &orchestrator.Action{Type: "job_failed"})
		if err != nil {
			t.Fatalf("job_failed from %s: %v", status, err)
		}
		if next.Status != orchestrator.TaskStatusAborted {
			t.Fatalf("expected aborted from %s, got %s", status, next.Status)
		}
	}
}

// ---- DefaultMachine: auto transitions from executing ----

func TestDefaultMachine_Executing_TasksReady_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"tasks":[{"title":"subtask"}]}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when tasks ready")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_Artifact_NoUnresolvedFindings_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	// artifact present, no executing-state findings
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when artifact present and no unresolved executing findings")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_Artifact_AllExecutingResolved_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"pr-verify":{"source_state":"executing","findings":[{"message":"CI passed","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to verifying when all executing findings resolved")
	}
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_Artifact_OpenExecutingFindings_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"pr-verify":{"source_state":"executing","findings":[{"message":"CI failed","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when executing findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_NoArtifact_NoAdvance(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	_, ok := sm.Advance(task)
	if ok {
		t.Fatal("expected no advance when no artifact and no tasks")
	}
}

// ---- DefaultMachine: auto transitions from verifying ----

func TestDefaultMachine_Verifying_OpenFindings_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"verify-gate":{"source_state":"verifying","findings":[{"message":"needs fix","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to reworking when verifying findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestDefaultMachine_Verifying_AllResolved_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"verify-gate":{"source_state":"verifying","findings":[{"message":"looks good","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when verifying findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Verifying_NoFindings_PassThrough_Done(t *testing.T) {
	// verify gate を持たない単純タスク: executing → verifying → done の pass-through
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusVerifying,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when no verifying-state findings (pass-through)")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

// ---- DefaultMachine: auto transitions from reworking ----

func TestDefaultMachine_Reworking_AllResolved_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"CI passed","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when all findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_OpenFindings_SelfLoop(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"pr-verify":{"source_state":"reworking","findings":[{"message":"CI still failing","status":"open"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected self-loop when unresolved findings in reworking")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking (self-loop), got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_NoFindings_Done(t *testing.T) {
	// 検証エントリが一切ない場合: NoUnresolvedFindings() = true → done
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://..."}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when no findings exist")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Reworking_MixedSourceStates_AnyOpenBlocksDone(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	// verifying-state entry still open, reworking-state resolved
	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://..."},
			"verification":{
				"gate-a":{"source_state":"verifying","findings":[{"message":"issue","status":"open"}]},
				"gate-b":{"source_state":"reworking","findings":[{"message":"ok","status":"resolved"}]}
			}
		}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected self-loop when any finding is unresolved across all sources")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking (self-loop), got %s", next.Status)
	}
}

// ---- DefaultMachine: AvailableActions ----

func TestDefaultMachine_AvailableActions_Pending(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusPending)
	want := map[string]bool{"start": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(pending) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(pending)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Executing(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusExecuting)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(executing) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(executing)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Verifying(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusVerifying)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(verifying) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(verifying)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Reworking(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusReworking)
	want := map[string]bool{"done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(reworking) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(reworking)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_DoneIsEmpty(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		if actions := sm.AvailableActions(status); len(actions) != 0 {
			t.Errorf("AvailableActions(%q) = %v, want empty", status, actions)
		}
	}
}

func TestDefaultMachine_AvailableActions_ExcludesJobFailed(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusReworking,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
	} {
		for _, a := range sm.AvailableActions(status) {
			if a == "job_failed" {
				t.Errorf("job_failed must not appear in AvailableActions(%q)", status)
			}
		}
	}
}

// ---- Generic StateMachine infrastructure tests ----

func TestStateMachine_Advance_ConditionMet(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{Action: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
				Condition: func(payload json.RawMessage) bool {
					var m map[string]json.RawMessage
					json.Unmarshal(payload, &m)
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
	if next.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", next.Status)
	}
}

func TestStateMachine_Apply_IgnoresConditionRules(t *testing.T) {
	sm := &orchestrator.StateMachine{
		Name: "test",
		Rules: []orchestrator.Rule{
			{
				FromStatus: "executing",
				ToStatus:   "verifying",
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

// TestJobCompletedNotAnAction verifies that job_completed does not trigger a
// state transition in DefaultMachine. State transitions driven by hook/gate job
// completion must happen exclusively through DispatchAndAdvance (condition-based
// auto-advance), not through sm.Apply.
func TestJobCompletedNotAnAction(t *testing.T) {
	sm := orchestrator.DefaultMachine()

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
	}

	for _, status := range statuses {
		task := &orchestrator.Task{Status: status}
		_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
		if err == nil {
			t.Errorf("job_completed from %q should not transition (got no error)", status)
		}
	}
}
