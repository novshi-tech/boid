package server

import (
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// ---- action_send actor stamping + child_specced workspace scoping ----
//
// These three tests used to live in boid_executor_task_wake_test.go
// alongside the (now-deleted, card machine v2 retired the whole Wake
// mechanism — docs/plans/suggestion-as-state-transition-impl.md §3) Wake
// tests. They pin behavior of BoidOpActionSend itself, which is unrelated to
// Wake and stays exactly as it was — restored here under their own name
// rather than lost as collateral damage of that file's deletion (PR #987
// review, HIGH 6).

// TestBoidBuiltinExecutor_ActionSend_StampsActorTask verifies `boid action
// send` (brokered) always stamps the calling task's own actor — this is the
// primitive both khi-style workspace push and any future proxy-Go task use
// to act on another task's behalf; an empty actor here would defeat 論点11
// AND — since card machine v2 (§3.2/§3.8) — is exactly the value the
// push-down defense (穴11) keys off to reject a non-human actor's direct
// card-transition push. If this ever stamped ActorHuman (or empty) instead,
// api-level tests that inject ctx directly would keep passing while khi
// silently regained the ability to push done/go/drop through the gateway.
func TestBoidBuiltinExecutor_ActionSend_StampsActorTask(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		// "done" is shared between Card and Execution — default to Execution
		// here per the card-model-cleanup PR-2 spec's disambiguation rule
		// (this fixture references none of the 8 execution-only fields and its
		// status is not one of the Card-only parked/working/dropped values).
		{ID: "t1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{TaskID: "caller-task", ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "t1",
		ActionType: "reopen",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	want := orchestrator.ActorTask("caller-task")
	if workflow.appliedActor != want {
		t.Fatalf("actor = %q, want %q", workflow.appliedActor, want)
	}
}

// TestBoidBuiltinExecutor_ActionSend_ChildSpecced_RejectsProjectOutsideWorkspace
// pins the codex review Blocker fix: child_specced's payload carries its own
// "project" field (the project accept(go) will later create/auto-start a
// real task in), independent of the parent card's own project. A workspace
// job must not be able to specc a child aimed at a project outside its own
// workspace scope.
func TestBoidBuiltinExecutor_ActionSend_ChildSpecced_RejectsProjectOutsideWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "card-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "card-1",
		ActionType: "child_specced",
		Payload:    []byte(`{"id":"c1","project":"proj-outside","behavior":"executor"}`),
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Fatalf("stderr = %q, want mention of workspace scoping", resp.Stderr)
	}
	if workflow.applyCallCount != 0 {
		t.Fatalf("ApplyAction must not be called when the child's project is outside the workspace, got %d calls", workflow.applyCallCount)
	}
}

// TestBoidBuiltinExecutor_ActionSend_ChildSpecced_AllowsProjectWithinWorkspace
// confirms the happy path is not broken by the new guard.
func TestBoidBuiltinExecutor_ActionSend_ChildSpecced_AllowsProjectWithinWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "card-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1", "proj-2"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:         sandbox.BoidOpActionSend,
		TaskID:     "card-1",
		ActionType: "child_specced",
		Payload:    []byte(`{"id":"c1","project":"proj-2","behavior":"executor"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if workflow.applyCallCount != 1 {
		t.Fatalf("ApplyAction call count = %d, want 1", workflow.applyCallCount)
	}
}
