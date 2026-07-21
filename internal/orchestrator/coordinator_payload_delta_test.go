package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR7 codex review Blocker 1 (wiring-seams.md #17): pins
// DispatchResult.PayloadDelta's exact contract at the Coordinator level —
// FinalPayload is a (potentially stale-by-completion-time) full snapshot,
// PayloadDelta is only what THIS cycle's hooks actually wrote. See
// internal/api's TestTaskWorkflowServiceRunDispatchLoop_
// PreservesMidHookRPCWrite_ReopenScenario for the end-to-end regression this
// enables at the persist layer.

// TestCoordinator_DispatchAndAdvance_PayloadDelta_EmptyWhenHookProducesNoOutput
// is the crux of the fix: a task that already has a payload (the reopen
// case) dispatching a hook that produces no file-based output (the common
// shape for an agent reporting exclusively via `boid task update
// --payload-patch`) must get an EMPTY delta, even though FinalPayload still
// carries the full pre-existing payload forward unchanged.
func TestCoordinator_DispatchAndAdvance_PayloadDelta_EmptyWhenHookProducesNoOutput(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", "", 0) // no output at all

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		// Pre-existing payload, as if the task had already been through a
		// prior dispatch round (e.g. reopen) before this one started.
		Payload: json.RawMessage(`{"artifact":{"report":{"summary":"pre-existing"}}}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "hook-a"}})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("DispatchAndAdvance: %v", err)
	}

	if string(result.PayloadDelta) != "{}" {
		t.Errorf("PayloadDelta = %s, want {} (hook produced no output)", result.PayloadDelta)
	}
	// FinalPayload still carries the pre-existing payload forward — this is
	// the value that must NOT be used to persist onto a freshly re-read row.
	var final map[string]json.RawMessage
	if err := json.Unmarshal(result.FinalPayload, &final); err != nil {
		t.Fatalf("FinalPayload not JSON: %v", err)
	}
	if _, ok := final["artifact"]; !ok {
		t.Errorf("FinalPayload should still carry the pre-existing payload forward, got %s", result.FinalPayload)
	}
}

// TestCoordinator_DispatchAndAdvance_PayloadDelta_OnlyContainsThisCyclesWrites
// verifies the positive case: when a hook DOES produce output, PayloadDelta
// contains only what that hook wrote — not the pre-existing payload it never
// touched.
func TestCoordinator_DispatchAndAdvance_PayloadDelta_OnlyContainsThisCyclesWrites(t *testing.T) {
	mock := newMockExecutorWaiter()
	mock.setHookCompletion("hook-a", `{"payload_patch":{"verification":{"passed":true}}}`, 0)

	eval := &orchestrator.Evaluator{}
	coord := &orchestrator.Coordinator{
		Evaluator:    eval,
		HookExecutor: mock,
		Waiter:       mock,
		MaxDepth:     5,
	}

	task := &orchestrator.Task{
		ID:        "01234567-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
		Payload:   json.RawMessage(`{"artifact":{"report":{"summary":"pre-existing, untouched by hook-a"}}}`),
	}
	meta := metaWithBehavior([]projectspec.Hook{{ID: "hook-a"}})
	sm := simpleStateMachine()

	result, err := coord.DispatchAndAdvance(context.Background(), task, meta, sm)
	if err != nil {
		t.Fatalf("DispatchAndAdvance: %v", err)
	}

	var delta map[string]json.RawMessage
	if err := json.Unmarshal(result.PayloadDelta, &delta); err != nil {
		t.Fatalf("PayloadDelta not JSON: %v", err)
	}
	if _, ok := delta["verification"]; !ok {
		t.Errorf("PayloadDelta should contain the hook's own write (verification), got %s", result.PayloadDelta)
	}
	if _, ok := delta["artifact"]; ok {
		t.Errorf("PayloadDelta should NOT contain the pre-existing artifact key the hook never touched, got %s", result.PayloadDelta)
	}
}
