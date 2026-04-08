package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRegistry_Resolve_KnownTransition(t *testing.T) {
	reg := orchestrator.NewDefaultRegistry()
	task := &orchestrator.Task{Transition: "one-shot"}

	sm, err := reg.Resolve(task)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sm.Name != "one-shot" {
		t.Fatalf("expected one-shot, got %s", sm.Name)
	}
}

func TestRegistry_Resolve_UnknownTransition(t *testing.T) {
	reg := orchestrator.NewDefaultRegistry()
	task := &orchestrator.Task{Transition: "unknown"}

	_, err := reg.Resolve(task)
	if err == nil {
		t.Fatal("expected error for unknown transition model")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestRegistry_Resolve_NonexistentTransitionModel(t *testing.T) {
	reg := orchestrator.NewDefaultRegistry()
	task := &orchestrator.Task{Transition: "nonexistent-machine"}

	_, err := reg.Resolve(task)
	if err == nil {
		t.Fatal("expected error for unknown transition model")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestOneShotMachine_PendingToExecutingToDone(t *testing.T) {
	sm := orchestrator.OneShotMachine()

	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}

	next, err := sm.Apply(task, &orchestrator.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}

	next, err = sm.Apply(next, &orchestrator.Action{Type: "done"})
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestOneShotMachine_InvalidTransition(t *testing.T) {
	sm := orchestrator.OneShotMachine()

	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending}

	_, err := sm.Apply(task, &orchestrator.Action{Type: "done"})
	if err == nil {
		t.Fatal("expected error for invalid transition pending -> done")
	}
	if !strings.Contains(err.Error(), "no transition") {
		t.Fatalf("expected no transition error, got: %v", err)
	}
}

func TestOneShotMachine_AbortFromAny(t *testing.T) {
	sm := orchestrator.OneShotMachine()

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
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

func TestFeedbackLoopMachine_FullCycle(t *testing.T) {
	sm := orchestrator.FeedbackLoopMachine()

	task := &orchestrator.Task{Status: orchestrator.TaskStatusPending, Payload: json.RawMessage(`{}`)}

	next, err := sm.Apply(task, &orchestrator.Action{Type: "start"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", next.Status)
	}

	next.Payload = json.RawMessage(`{"artifact":{"pr_url":"https://..."}}`)
	advanced, ok := sm.Advance(next)
	if !ok {
		t.Fatal("expected advance from executing to verifying")
	}
	if advanced.Status != orchestrator.TaskStatusVerifying {
		t.Fatalf("expected verifying, got %s", advanced.Status)
	}

	advanced.Payload = json.RawMessage(`{"artifact":{"pr_url":"https://..."},"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]},"review":{"source_state":"verifying","findings":[{"message":"clean","status":"resolved"}]}}}`)
	next2, ok := sm.Advance(advanced)
	if !ok {
		t.Fatal("expected advance from verifying to in_review")
	}
	if next2.Status != orchestrator.TaskStatusInReview {
		t.Fatalf("expected in_review, got %s", next2.Status)
	}

	next3, err := sm.Apply(next2, &orchestrator.Action{Type: "collect_feedback"})
	if err != nil {
		t.Fatalf("collect_feedback: %v", err)
	}
	if next3.Status != orchestrator.TaskStatusCollectingFeedback {
		t.Fatalf("expected collecting_feedback, got %s", next3.Status)
	}

	next3.Payload = json.RawMessage(`{"artifact":{"pr_url":"https://..."},"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]},"pr-review":{"source_state":"collecting_feedback","findings":[{"message":"fix error handling","status":"open"}]}}}`)
	next4, ok := sm.Advance(next3)
	if !ok {
		t.Fatal("expected advance from collecting_feedback to executing")
	}
	if next4.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing after rework, got %s", next4.Status)
	}

	next3.Payload = json.RawMessage(`{"artifact":{"pr_url":"https://..."},"verification":{"ci":{"source_state":"verifying","findings":[{"message":"tests pass","status":"resolved"}]},"pr-review":{"source_state":"collecting_feedback","findings":[{"message":"looks good","status":"resolved"}]}}}`)
	next5, ok := sm.Advance(next3)
	if !ok {
		t.Fatal("expected advance from collecting_feedback to done")
	}
	if next5.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next5.Status)
	}
}

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

func TestOneShotFeedbackMachine_NoVerification_Done(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected advance to done when artifact present and no verification")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestOneShotFeedbackMachine_AllFindingsResolved_Done(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"executing",
					"findings":[{"message":"GitHub Actions passed","status":"resolved"}]
				}
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

func TestOneShotFeedbackMachine_OpenFindings_ToReworking(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"executing",
					"findings":[{"message":"GitHub Actions failed: test","status":"open"}]
				}
			}
		}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected transition to reworking when findings open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking, got %s", next.Status)
	}
}

func TestOneShotFeedbackMachine_Reworking_OpenFindings_SelfLoop(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"reworking",
					"findings":[{"message":"GitHub Actions failed: test","status":"open"}]
				}
			}
		}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected self-loop in reworking when findings still open")
	}
	if next.Status != orchestrator.TaskStatusReworking {
		t.Fatalf("expected reworking (self-loop), got %s", next.Status)
	}
}

func TestOneShotFeedbackMachine_Reworking_AllResolved_Done(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status: orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{
			"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"},
			"verification":{
				"github-pr-verification/pr-verify":{
					"source_state":"reworking",
					"findings":[{"message":"GitHub Actions passed","status":"resolved"}]
				}
			}
		}`),
	}

	next, ok := sm.Advance(task)
	if !ok {
		t.Fatal("expected transition to done when reworking findings resolved")
	}
	if next.Status != orchestrator.TaskStatusDone {
		t.Fatalf("expected done, got %s", next.Status)
	}
}

func TestOneShotFeedbackMachine_Reworking_NoFindings_NoAdvance(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	// reworking 直後はまだ gate が発火していないので findings がない → Advance は false を返す
	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusReworking,
		Payload: json.RawMessage(`{"artifact":{"pr_url":"https://github.com/owner/repo/pull/1"}}`),
	}

	_, ok := sm.Advance(task)
	if ok {
		t.Fatal("expected no advance in reworking when no reworking-state findings yet")
	}
}

func TestOneShotFeedbackMachine_NoArtifact_NoAdvance(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

	task := &orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: json.RawMessage(`{}`),
	}

	_, ok := sm.Advance(task)
	if ok {
		t.Fatal("expected no advance when no artifact")
	}
}

func TestOneShotFeedbackMachine_TasksReady_Done(t *testing.T) {
	sm := orchestrator.OneShotFeedbackMachine()

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

func TestFeedbackLoopMachine_AbortFromAny(t *testing.T) {
	sm := orchestrator.FeedbackLoopMachine()

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusVerifying,
		orchestrator.TaskStatusInReview,
		orchestrator.TaskStatusCollectingFeedback,
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

func TestStateMachine_AvailableActions_OneShotPending(t *testing.T) {
	sm := orchestrator.OneShotMachine()
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

func TestStateMachine_AvailableActions_OneShotExecuting(t *testing.T) {
	sm := orchestrator.OneShotMachine()
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

func TestStateMachine_AvailableActions_ExcludesJobFailed(t *testing.T) {
	for _, sm := range []*orchestrator.StateMachine{
		orchestrator.OneShotMachine(),
		orchestrator.OneShotFeedbackMachine(),
		orchestrator.FeedbackLoopMachine(),
	} {
		for _, status := range []orchestrator.TaskStatus{
			orchestrator.TaskStatusPending,
			orchestrator.TaskStatusExecuting,
			orchestrator.TaskStatusDone,
			orchestrator.TaskStatusAborted,
		} {
			for _, a := range sm.AvailableActions(status) {
				if a == "job_failed" {
					t.Errorf("machine %q: job_failed must not appear in AvailableActions(%q)", sm.Name, status)
				}
			}
		}
	}
}

func TestStateMachine_AvailableActions_DoneIsEmpty(t *testing.T) {
	for _, sm := range []*orchestrator.StateMachine{
		orchestrator.OneShotMachine(),
		orchestrator.OneShotFeedbackMachine(),
		orchestrator.FeedbackLoopMachine(),
	} {
		for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
			if actions := sm.AvailableActions(status); len(actions) != 0 {
				t.Errorf("machine %q: AvailableActions(%q) = %v, want empty", sm.Name, status, actions)
			}
		}
	}
}

func TestStateMachine_AvailableActions_FeedbackLoopInReview(t *testing.T) {
	sm := orchestrator.FeedbackLoopMachine()
	actions := sm.AvailableActions(orchestrator.TaskStatusInReview)
	want := map[string]bool{"collect_feedback": true, "abort": true}
	if len(actions) != len(want) {
		t.Fatalf("AvailableActions(in_review) = %v, want %v", actions, want)
	}
	for _, a := range actions {
		if !want[a] {
			t.Errorf("unexpected action %q", a)
		}
	}
}

func TestGetMachine_KnownTransitions(t *testing.T) {
	for _, name := range []string{"one-shot", "one-shot-feedback", "feedback-loop"} {
		sm, ok := orchestrator.GetMachine(name)
		if !ok {
			t.Errorf("GetMachine(%q) not found", name)
			continue
		}
		if sm.Name != name {
			t.Errorf("GetMachine(%q).Name = %q, want %q", name, sm.Name, name)
		}
	}
}

func TestGetMachine_Unknown(t *testing.T) {
	_, ok := orchestrator.GetMachine("nonexistent")
	if ok {
		t.Error("GetMachine(nonexistent) should return ok=false")
	}
}

// TestJobCompletedNotAnAction verifies that job_completed does not trigger a
// state transition in any machine. State transitions driven by hook/gate job
// completion must happen exclusively through DispatchAndAdvance (condition-based
// auto-advance), not through sm.Apply.
func TestJobCompletedNotAnAction(t *testing.T) {
	machines := []*orchestrator.StateMachine{
		orchestrator.OneShotMachine(),
		orchestrator.OneShotFeedbackMachine(),
		orchestrator.FeedbackLoopMachine(),
	}

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusExecuting,
		orchestrator.TaskStatusReworking,
	}

	for _, sm := range machines {
		for _, status := range statuses {
			task := &orchestrator.Task{Status: status}
			_, err := sm.Apply(task, &orchestrator.Action{Type: "job_completed"})
			if err == nil {
				t.Errorf("machine %q: job_completed from %q should not transition (got no error)", sm.Name, status)
			}
		}
	}
}
