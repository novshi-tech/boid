package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Manual transitions ----

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

func TestDefaultMachine_Reopen_DoneToExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusDone}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
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

func TestDefaultMachine_ExecutingToAwaiting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "ask"})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if next.Status != orchestrator.TaskStatusAwaiting {
		t.Fatalf("expected awaiting, got %s", next.Status)
	}
}

func TestDefaultMachine_AwaitingToExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAwaiting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "answer"})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

// TestDefaultMachine_AwaitingToDone verifies that a parent supervisor can
// approve a child's `done_request` (= terminate the awaiting child) via
// `boid action send --type done`. The transition is the canonical
// down-action documented in docs/plans/lifecycle-accountability.md and
// boid-supervisor/SKILL.md; without it the supervisor falls back to
// `boid task answer` which forces a wasteful agent re-spawn.
func TestDefaultMachine_AwaitingToDone(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAwaiting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done from awaiting: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

// TestDefaultMachine_FailFromExecuting verifies that an executor reporting an
// unrecoverable failure transitions directly to aborted. The `fail` action is
// the up-event canonical counterpart of `done` from executing: the agent
// self-reports the outcome and the parent supervisor's polling decides
// recovery (reopen / leave aborted). Symmetric to `done: executing → done`.
func TestDefaultMachine_FailFromExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "fail"})
	if err != nil {
		t.Fatalf("fail from executing: %v", err)
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("expected aborted, got %s", next.Status)
	}
}

// TestDefaultMachine_Reopen_AbortedToExecuting verifies that the parent
// supervisor can recover a failed child via `boid task reopen` — symmetric
// to reopen from done. Without this transition `--fail` would be a dead-end
// and failure_report's "Recoverable with a hint" path could not be expressed
// without first un-aborting through some other mechanism.
func TestDefaultMachine_Reopen_AbortedToExecuting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusAborted}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen from aborted: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}
}

func TestDefaultMachine_InvalidTransition_PendingToAwaiting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "ask"})
	if err == nil {
		t.Fatal("expected error: ask from pending is invalid")
	}
}

func TestDefaultMachine_Abort_FromAnyState(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
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
		orchestrator.TaskStatusAwaiting,
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

// ---- Auto transitions ----

func TestDefaultMachine_Executing_LifecycleExecuted_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"lifecycle":{"executed":true}}`),
	}
	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when lifecycle.executed=true")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestDefaultMachine_Executing_NoLifecycleExecuted_NoTransition(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}
	if _, ok := sm.Advance(task); ok {
		t.Fatal("expected no advance when lifecycle.executed not set")
	}
}

// ---- AvailableActions ----

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
	want := map[string]bool{"done": true, "fail": true, "ask": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(executing) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(executing)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Awaiting(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusAwaiting)
	want := map[string]bool{"answer": true, "done": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(awaiting) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(awaiting)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_Done(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusDone)
	want := map[string]bool{"reopen": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(done) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(done)", a)
		}
	}
}

// TestDefaultMachine_AvailableActions_Aborted verifies that aborted tasks
// can be reopened. This is the recovery path for `--fail` (executing →
// aborted): supervisor inspects the failure_report, then either reopens
// with a hint or leaves the task aborted as final.
func TestDefaultMachine_AvailableActions_Aborted(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusAborted)
	want := map[string]bool{"reopen": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(aborted) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q in AvailableActions(aborted)", a)
		}
	}
}

func TestDefaultMachine_AvailableActions_ExcludesJobFailed(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	for _, status := range []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusAwaiting,
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

// ---- Generic StateMachine infrastructure ----

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

// TestJobCompletedNotAnAction verifies that job_completed does not trigger a
// state transition. State transitions driven by hook completion happen via
// auto-advance (lifecycle.executed condition), not through sm.Apply.
func TestJobCompletedNotAnAction(t *testing.T) {
	sm := orchestrator.DefaultMachine()
	task := &orchestrator.Task{Status: orchestrator.TaskStatusExecuting}
	_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
	if err == nil {
		t.Errorf("job_completed should not transition (got no error)")
	}
}
