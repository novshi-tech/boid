package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file pins ReplayHook and ListHooksForStatus's switch from bare
// Meta.Get to Meta.GetWithWorkspace (docs/plans/workspace-default-project.md
// §PR分割案 PR2, §現状の実測4's "hook replay" / "hook 一覧" rows — the
// remaining 2 of the 5 mandatory-switch call sites besides task_create.go).
//
// Unlike task_create.go's CreateTask (which tolerates a nil meta and so
// falls back to bare Get on any hydration error), these two already 500 on
// a bare-Get miss today — so the doc's own reasoning applies directly here
// ("方向は揃う"): a straight swap to GetWithWorkspace, no fallback, keeps
// "meta not loaded" 500ing exactly as before, and simply widens the set of
// conditions that can trigger that same 500 (workspace.yaml corrupt /
// host_commands conflict — impossible under bare Get, so there is no prior
// behavior to preserve for them specifically).

// metaRecordingCoordinator is a DispatchCoordinator double that records the
// *ProjectMeta it was called with, so a test can prove which of Get /
// GetWithWorkspace's return value actually reached the coordinator (the two
// otherwise-identical stubMetaStore return values would be indistinguishable
// without this).
type metaRecordingCoordinator struct {
	lastReplayMeta *orchestrator.ProjectMeta
}

func (c *metaRecordingCoordinator) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	var payload json.RawMessage
	if task.Exec != nil {
		payload = task.Exec.Payload
	}
	return &orchestrator.DispatchResult{FinalPayload: payload}, nil
}

func (c *metaRecordingCoordinator) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	c.lastReplayMeta = meta
	var payload json.RawMessage
	if task.Exec != nil {
		payload = task.Exec.Payload
	}
	return &orchestrator.ReplayResult{FinalPayload: payload}, nil
}

func TestTaskWorkflowService_ReplayHook_UsesWorkspaceHydratedMeta(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-hydrate-replay",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev", Payload: json.RawMessage(`{}`)},
	}
	bareMeta := &orchestrator.ProjectMeta{ID: "bare", TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}
	hydratedMeta := &orchestrator.ProjectMeta{ID: "hydrated", TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}
	coord := &metaRecordingCoordinator{}
	txStore := &recordingTxStore{task: task}
	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task},
		Jobs:        &stubJobStore{},
		Meta:        stubMetaStore{meta: bareMeta, hydrated: hydratedMeta},
		Coordinator: coord,
		Tx:          recordingTransactor{store: txStore},
	}

	if _, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"}); err != nil {
		t.Fatalf("ReplayHook() error = %v", err)
	}
	if coord.lastReplayMeta != hydratedMeta {
		t.Errorf("coordinator received meta %+v (id=%q), want the workspace-hydrated meta (id=%q)",
			coord.lastReplayMeta, coord.lastReplayMeta.ID, hydratedMeta.ID)
	}
}

func TestTaskWorkflowService_ReplayHook_MetaNotLoaded500(t *testing.T) {
	task := &orchestrator.Task{ID: "task-hydrate-replay-2", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Jobs:  &stubJobStore{},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"})
	if err == nil {
		t.Fatal("expected 500 for a project with no cached meta, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("error = %v, want *StatusError{500}", err)
	}
}

// TestTaskWorkflowService_ReplayHook_WorkspaceHydrationError500 pins the NEW
// failure surface: a workspace.yaml load failure / host_commands conflict
// now also 500s here, even though the project itself has a perfectly good
// cached meta (bare Get would have succeeded). This could never happen
// under the pre-PR2 bare-Get-only code — it is an accepted, intentional
// widening (doc's own words: "方向は揃うが...変化が入り得る"), pinned here so
// it reads as designed rather than an accident.
func TestTaskWorkflowService_ReplayHook_WorkspaceHydrationError500(t *testing.T) {
	task := &orchestrator.Task{ID: "task-hydrate-replay-3", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Jobs:  &stubJobStore{},
		Meta: stubMetaStore{
			meta:       &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}},
			hydrateErr: errors.New("workspace.yaml: yaml: line 3: did not find expected key"),
		},
	}

	_, err := svc.ReplayHook(context.Background(), task.ID, ReplayHookRequest{HookID: "main-hook"})
	if err == nil {
		t.Fatal("expected 500 when workspace hydration fails, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("error = %v, want *StatusError{500}", err)
	}
}

func TestTaskWorkflowService_ListHooksForStatus_UsesWorkspaceHydratedMeta(t *testing.T) {
	task := &orchestrator.Task{ID: "task-hydrate-list", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	bareMeta := &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}
	hydratedMeta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Hooks: []orchestrator.Hook{{ID: "workspace-only-hook", Command: "true"}}},
		},
	}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: bareMeta, hydrated: hydratedMeta},
	}

	hooks, err := svc.ListHooksForStatus(task.ID, "")
	if err != nil {
		t.Fatalf("ListHooksForStatus() error = %v", err)
	}
	found := false
	for _, h := range hooks {
		if h.ID == "workspace-only-hook" {
			found = true
		}
	}
	if !found {
		t.Errorf("hooks = %+v, want to include the workspace-only hook (proves GetWithWorkspace, not bare Get, was used)", hooks)
	}
}

func TestTaskWorkflowService_ListHooksForStatus_MetaNotLoaded500(t *testing.T) {
	task := &orchestrator.Task{ID: "task-hydrate-list-2", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: nil},
	}

	_, err := svc.ListHooksForStatus(task.ID, "")
	if err == nil {
		t.Fatal("expected 500 for a project with no cached meta, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("error = %v, want *StatusError{500}", err)
	}
}

func TestTaskWorkflowService_ListHooksForStatus_WorkspaceHydrationError500(t *testing.T) {
	task := &orchestrator.Task{ID: "task-hydrate-list-3", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Meta: stubMetaStore{
			meta:       &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}},
			hydrateErr: errors.New("host_commands: builtin conflict"),
		},
	}

	_, err := svc.ListHooksForStatus(task.ID, "")
	if err == nil {
		t.Fatal("expected 500 when workspace hydration fails, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("error = %v, want *StatusError{500}", err)
	}
}
