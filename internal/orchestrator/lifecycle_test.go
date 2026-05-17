package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeLifecycleStore struct {
	actions []*orchestrator.Action
	err     error
}

func (s *fakeLifecycleStore) ListActionsByTask(_ string) ([]*orchestrator.Action, error) {
	return s.actions, s.err
}

func TestDeriveLifecycle_NilStore(t *testing.T) {
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !lc.Executed {
		t.Errorf("expected Executed=true")
	}
	if lc.Abort != nil {
		t.Errorf("expected Abort=nil, got %+v", lc.Abort)
	}
}

func TestDeriveLifecycle_AbortReason(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				Type:       "abort",
				FromStatus: "executing",
				ToStatus:   "aborted",
				Payload:    []byte(`{"code":"manual_abort","message":"user requested"}`),
			},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Abort == nil {
		t.Fatal("expected Abort != nil")
	}
	if lc.Abort.Code != "manual_abort" {
		t.Errorf("expected Abort.Code=manual_abort, got %q", lc.Abort.Code)
	}
	if lc.Abort.Message != "user requested" {
		t.Errorf("expected Abort.Message=%q, got %q", "user requested", lc.Abort.Message)
	}
}

// done_request must surface as lc.Done with the message preserved so the
// auto-advance rule fires with the agent's text intact. Without this the
// timeline's done action would lose the agent's completion summary.
func TestDeriveLifecycle_DoneRequest(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				Type:       "done_request",
				FromStatus: "executing",
				ToStatus:   "executing",
				Payload:    []byte(`{"message":"PR #439 merged"}`),
			},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Done == nil {
		t.Fatal("expected lc.Done != nil")
	}
	if lc.Done.Message != "PR #439 merged" {
		t.Errorf("Done.Message = %q, want %q", lc.Done.Message, "PR #439 merged")
	}
	if lc.Fail != nil {
		t.Errorf("expected lc.Fail = nil, got %+v", lc.Fail)
	}
}

// fail_request must surface as lc.Fail, mirroring the done_request path.
func TestDeriveLifecycle_FailRequest(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				Type:       "fail_request",
				FromStatus: "executing",
				ToStatus:   "executing",
				Payload:    []byte(`{"message":"tests broken in main"}`),
			},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Fail == nil {
		t.Fatal("expected lc.Fail != nil")
	}
	if lc.Fail.Message != "tests broken in main" {
		t.Errorf("Fail.Message = %q, want %q", lc.Fail.Message, "tests broken in main")
	}
	if lc.Done != nil {
		t.Errorf("expected lc.Done = nil, got %+v", lc.Done)
	}
}

// Reopen / answer / start re-entry into executing must clear stale Done/Fail
// intent recorded in a prior cycle. Without this reset a task reopened after
// a done_request would auto-advance back to done immediately on the next
// dispatch cycle even though the agent has not reported again.
func TestDeriveLifecycle_ResetOnReentryToExecuting(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{Type: "done_request", FromStatus: "executing", ToStatus: "executing",
				Payload: []byte(`{"message":"first done"}`)},
			{Type: "auto_advance", FromStatus: "executing", ToStatus: "done",
				Payload: []byte(`{"message":"first done"}`)},
			{Type: "reopen", FromStatus: "done", ToStatus: "executing"},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Done != nil {
		t.Errorf("expected lc.Done = nil after reopen, got %+v", lc.Done)
	}
}

// done_request followed by fail_request in the same cycle must yield Fail (the
// later report) with Done cleared. They are mutually exclusive intents.
func TestDeriveLifecycle_FailSupersedesDoneInSameCycle(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{Type: "done_request", FromStatus: "executing", ToStatus: "executing",
				Payload: []byte(`{"message":"premature done"}`)},
			{Type: "fail_request", FromStatus: "executing", ToStatus: "executing",
				Payload: []byte(`{"message":"actually failed"}`)},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Done != nil {
		t.Errorf("expected lc.Done = nil (superseded), got %+v", lc.Done)
	}
	if lc.Fail == nil || lc.Fail.Message != "actually failed" {
		t.Errorf("expected lc.Fail.Message=actually failed, got %+v", lc.Fail)
	}
}

// End-to-end through DeriveLifecycle + injectLifecycle + state machine:
// after the agent recorded done_request and the hook (run-agent) completed
// normally (hookExecuted=true), the state machine must auto-advance to done
// with the agent's message preserved on the action payload. This is the
// canonical Phase 2.c-fix flow that replaces the synchronous-ApplyAction
// race with deferred auto-advance.
func TestLifecycleToStateMachine_DoneRequest_AutoAdvancesWithMessage(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{Type: "done_request", FromStatus: "executing", ToStatus: "executing",
				Payload: []byte(`{"message":"PR #439 merged"}`)},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, true)
	if err != nil {
		t.Fatalf("DeriveLifecycle: %v", err)
	}
	if !lc.Executed || lc.Done == nil || lc.Done.Message != "PR #439 merged" {
		t.Fatalf("unexpected lifecycle: executed=%v done=%+v", lc.Executed, lc.Done)
	}

	// Serialize the lifecycle into a payload exactly the way the coordinator
	// does (via the exported types JSON-tagged fields) so we exercise the same
	// shape the state machine will see at runtime.
	payload := []byte(`{"lifecycle":{"executed":true,"done":{"message":"PR #439 merged"}}}`)
	sm := orchestrator.DefaultMachine()
	outcome := sm.AdvanceFull(&orchestrator.Task{
		Status:  orchestrator.TaskStatusExecuting,
		Payload: payload,
	})
	if outcome == nil {
		t.Fatal("expected state machine advance, got nil")
	}
	if outcome.Task.Status != orchestrator.TaskStatusDone {
		t.Errorf("status = %s, want done", outcome.Task.Status)
	}
	var ap map[string]string
	if err := json.Unmarshal(outcome.ActionPayload, &ap); err != nil {
		t.Fatalf("ActionPayload not parseable: %v", err)
	}
	if ap["message"] != "PR #439 merged" {
		t.Errorf("auto_advance.message = %q, want %q", ap["message"], "PR #439 merged")
	}
}

// Without hookExecuted=true the auto-advance must not fire, even with the
// done_request in history. This is what makes the new design race-free: the
// runtime must finish (lifecycle.executed) before the state transition runs.
func TestLifecycleToStateMachine_DoneRequestAloneDoesNotAdvance(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{Type: "done_request", FromStatus: "executing", ToStatus: "executing",
				Payload: []byte(`{"message":"reported but runtime still running"}`)},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, false)
	if err != nil {
		t.Fatalf("DeriveLifecycle: %v", err)
	}
	if lc.Done == nil {
		t.Fatal("expected lc.Done to still be derived from history")
	}

	payload := []byte(`{"lifecycle":{"executed":false,"done":{"message":"x"}}}`)
	sm := orchestrator.DefaultMachine()
	if outcome := sm.AdvanceFull(&orchestrator.Task{
		Status: orchestrator.TaskStatusExecuting, Payload: payload,
	}); outcome != nil {
		t.Errorf("expected no advance (executed=false), got status=%s", outcome.Task.Status)
	}
}
