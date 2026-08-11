package server

import (
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// ---- Phase 1 PR-4 (docs/plans/cross-project-issue-triage.md 論点10):
// brokered wake op ----

// TestBoidBuiltinExecutor_TaskWake_CallsWorkflowWake pins the happy path: a
// sandboxed caller within its own workspace can trigger Wake() through the
// brokered op — this is the source-event wake category's execution口 (khi
// judged + nose approved, no daemon judgment involved).
func TestBoidBuiltinExecutor_TaskWake_CallsWorkflowWake(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusParked},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskWake,
		TaskID: "t1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if workflow.applyCallCount != 1 {
		t.Fatalf("Wake (via ApplyAction stub) call count = %d, want 1", workflow.applyCallCount)
	}
	if workflow.appliedTaskID != "t1" {
		t.Fatalf("appliedTaskID = %q, want t1", workflow.appliedTaskID)
	}
}

// TestBoidBuiltinExecutor_TaskWake_StampsActorTask verifies 論点11's brokered
// wake path — the代行タスク path this field exists for — records
// orchestrator.ActorTask(ctx.TaskID), the CALLING task, not an empty actor.
func TestBoidBuiltinExecutor_TaskWake_StampsActorTask(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusParked},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{TaskID: "proxy-go-task", ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskWake,
		TaskID: "t1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	want := orchestrator.ActorTask("proxy-go-task")
	if workflow.appliedActor != want {
		t.Fatalf("actor = %q, want %q", workflow.appliedActor, want)
	}
}

// TestBoidBuiltinExecutor_ActionSend_StampsActorTask verifies `boid action
// send` (brokered) always stamps the calling task's own actor — this is the
// primitive both khi-style workspace push and any future proxy-Go task use
// to act on another task's behalf; an empty actor here would defeat 論点11.
func TestBoidBuiltinExecutor_ActionSend_StampsActorTask(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusReady},
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

// TestBoidBuiltinExecutor_TaskWake_RejectsOutsideWorkspace confirms the
// brokered wake op is scoped by AllowsProject the same way action_send is —
// a sandboxed caller cannot wake a task belonging to a project outside its
// own workspace.
func TestBoidBuiltinExecutor_TaskWake_RejectsOutsideWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-outside", Status: orchestrator.TaskStatusParked},
	}}
	workflow := &recordingWorkflow{}
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskWake,
		TaskID: "t1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "workspace") {
		t.Fatalf("stderr = %q, want mention of workspace scoping", resp.Stderr)
	}
	if workflow.applyCallCount != 0 {
		t.Fatalf("Wake must not be called when workspace scoping rejects the request, got %d calls", workflow.applyCallCount)
	}
}

// TestBoidBuiltinExecutor_TaskWake_RequiresTaskID confirms the missing-taskID
// guard fires before any store/workflow interaction.
func TestBoidBuiltinExecutor_TaskWake_RequiresTaskID(t *testing.T) {
	exec := &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: &capturingTaskStore{}},
		workflow: &recordingWorkflow{},
	}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskWake})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
}

// TestBoidBuiltinExecutor_ActionSend_ChildSpecced_RejectsProjectOutsideWorkspace
// pins the codex review Blocker fix: child_specced's payload carries its own
// "project" field (the project TaskWorkflowService.Dispatch will later
// create/auto-start a real task in), independent of the parent card's own
// project. A workspace job must not be able to specc a child aimed at a
// project outside its own workspace scope.
func TestBoidBuiltinExecutor_ActionSend_ChildSpecced_RejectsProjectOutsideWorkspace(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "card-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking},
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
		{ID: "card-1", ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking},
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

// TestBoidBuiltinExecutor_ActionSend_RoutesWakeActionsThroughApplyAction
// confirms boid_executor's BoidOpActionSend case has no special-casing for
// wake_triaged/wake_ready: it forwards them to ApplyAction exactly like any
// other action type, with zero pre-filtering. Combined with
// TestTaskWorkflowServiceApplyAction_RejectsInternalOnlyWakeActions
// (internal/api, which exercises the REAL TaskWorkflowService.ApplyAction —
// not a stub — and confirms it 400s these two names via
// StateMachine.IsManualAction), this pins the full chain: the new
// BoidOpTaskWake op (tested above) is NOT a bypass of that guard, because
// the guard lives entirely on the ApplyAction side that BoidOpActionSend
// still routes through unmodified — the ONLY way to wake a parked task from
// inside the sandbox is BoidOpTaskWake.
func TestBoidBuiltinExecutor_ActionSend_RoutesWakeActionsThroughApplyAction(t *testing.T) {
	if orchestrator.DefaultMachine().IsManualAction("wake_triaged") || orchestrator.DefaultMachine().IsManualAction("wake_ready") {
		t.Fatal("wake_triaged/wake_ready must stay Manual:false in the real machine (see machine.go)")
	}
	for _, actionType := range []string{"wake_triaged", "wake_ready"} {
		store := &capturingTaskStore{created: []*orchestrator.Task{
			{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusParked},
		}}
		workflow := &recordingWorkflow{}
		exec := &boidBuiltinExecutor{
			tasks:    &api.TaskAppService{Tasks: store},
			workflow: workflow,
		}
		ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

		exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
			Op:         sandbox.BoidOpActionSend,
			TaskID:     "t1",
			ActionType: actionType,
		})
		if workflow.appliedReq.Type != actionType {
			t.Fatalf("action_send(%s): expected it to route through ApplyAction unmodified, got req=%+v", actionType, workflow.appliedReq)
		}
	}
}
