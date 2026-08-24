package api

// docs/plans/ingestion-identity.md PR-1 (B-1): the drop side effect (I-6).
// `drop` releases a task's identity bindings so the same external keys can
// be re-linked to a fresh task later; `done` (I-5) must NOT — done holds
// identities. This is wired into TaskWorkflowService.ApplyAction's WithinTx
// switch (workflow_action.go), not machine.go — the state machine stays a
// pure transition table with zero side effects.

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestTaskWorkflowServiceApplyAction_Drop_ReleasesIdentityBindings(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusParked, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	// drop is a card-lifecycle transition (穴11 push-down defense,
	// workflow_action.go) — actor must be human.
	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "drop"})
	if err != nil {
		t.Fatalf("ApplyAction(drop): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDropped {
		t.Fatalf("status = %q, want dropped", result.Task.Status)
	}
	if len(txStore.unlinkAllForTaskCalls) != 1 || txStore.unlinkAllForTaskCalls[0] != task.ID {
		t.Fatalf("UnlinkAllForTask calls = %v, want exactly one call with task id %q", txStore.unlinkAllForTaskCalls, task.ID)
	}
}

// TestTaskWorkflowServiceApplyAction_Done_DoesNotReleaseIdentityBindings pins
// I-5: identity bindings survive a task reaching done. "done" is the ONE
// other manual, side-effect-free transition in machine.go (executing ->
// done) reachable through this same ApplyAction entry point, so it is the
// most direct way to prove the wiring in workflow_action.go's WithinTx
// switch calls UnlinkAllForTask for drop ONLY.
//
// Deliberately NOT newTriageWorkflowService here (unlike every other test in
// this file): that helper seeds a task_triage row so machineFor picks
// NewCardMachine (PR-B) — correct for drop (a card action) above, but wrong
// for this test's ordinary EXECUTING task, whose "done" belongs to
// NewExecutionMachine. Card machine v2 DOES have its own "done" rule
// (working→done, orchestrator.IsCardTransitionAction) — the two "done"s are
// disjoint by FromStatus (executing vs working), which is exactly why this
// test must stay on the execution-machine wiring to prove the identity-drop
// side effect is keyed off the ACTION being "drop" specifically, not off
// "which machine happened to be selected".
func TestTaskWorkflowServiceApplyAction_Done_DoesNotReleaseIdentityBindings(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusExecuting, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
	}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "done"})
	if err != nil {
		t.Fatalf("ApplyAction(done): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusDone {
		t.Fatalf("status = %q, want done", result.Task.Status)
	}
	if len(txStore.unlinkAllForTaskCalls) != 0 {
		t.Fatalf("UnlinkAllForTask calls = %v, want none (done must hold identities, I-5)", txStore.unlinkAllForTaskCalls)
	}
}
