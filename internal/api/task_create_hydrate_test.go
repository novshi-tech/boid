package api

import (
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file pins CreateTask's switch from bare Meta.Get to
// Meta.GetWithWorkspace (docs/plans/workspace-default-project.md §PR分割案
// PR2, §現状の実測4's "task 作成" row) — the first of the 5 mandatory-switch
// call sites. TestTaskAppServiceCreateTask_ProjectNotInMeta_Skips
// (service_test.go) already pins the "meta 未ロード → nil meta, continue"
// leg of the failure-mode table; the two tests here pin the other leg
// (hydration actually used when available) and the two NEW failure modes
// GetWithWorkspace can produce that bare Get never could (workspace load
// failure / host_commands conflict) — both must fall back to the bare meta
// rather than fail task creation, since neither could ever happen under the
// pre-PR2 code path.

// TestTaskAppServiceCreateTask_UsesWorkspaceHydratedMeta proves CreateTask
// now actually calls GetWithWorkspace rather than bare Get: the bare meta
// lacks the "extra" behavior, the hydrated one has it. Under the pre-PR2
// code (bare Get only) this request would 400 with `behavior "extra" not
// found`; post-PR2 it must succeed.
func TestTaskAppServiceCreateTask_UsesWorkspaceHydratedMeta(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{
			meta: &orchestrator.ProjectMeta{
				TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}},
			},
			hydrated: &orchestrator.ProjectMeta{
				TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}, "extra": {}},
			},
		},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "workspace-only behavior",
		Behavior:  "extra",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (workspace-hydrated meta should have supplied the \"extra\" behavior)", err)
	}
	if task == nil || task.Behavior != "extra" {
		t.Fatalf("task = %+v, want Behavior=extra", task)
	}
}

// TestTaskAppServiceCreateTask_HydrationErrorFallsBackToBareMeta simulates
// the two failure modes real GetWithWorkspace has that bare Get never could
// (a corrupt workspace.yaml, a host_commands conflict — §現状の実測4's
// failure-mode table rows 2/3, both "起きない" under bare Get). CreateTask
// must degrade to the bare meta rather than fail outright: these are new
// failure surfaces PR2 introduces by switching to GetWithWorkspace, and
// task creation staying available (with the project's own project.yaml
// behaviors, just without workspace-level enrichment) is the documented
// choice — the same "fall back to bare meta on hydration error" idiom
// already used by ProjectAppService.hydrateProjectWithWorkspace
// (project_service.go) and TaskWorkflowService.ApplyAction
// (workflow_action.go).
func TestTaskAppServiceCreateTask_HydrationErrorFallsBackToBareMeta(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{
			meta: &orchestrator.ProjectMeta{
				TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}},
			},
			hydrateErr: errors.New("workspace.yaml: yaml: line 3: did not find expected key"),
		},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "hydration error still creates the task",
		Behavior:  "dev",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (must fall back to the bare project.yaml meta)", err)
	}
	if task == nil || task.Behavior != "dev" {
		t.Fatalf("task = %+v, want Behavior=dev", task)
	}
}

// TestTaskAppServiceCreateTask_HydrationErrorAndNoBareMeta_NilMetaContinues
// is the corner where BOTH lookups fail: workspace hydration errors AND the
// project has no cached meta at all. This must land exactly where
// TestTaskAppServiceCreateTask_ProjectNotInMeta_Skips (service_test.go)
// already lands — nil meta, task creation still succeeds (ResolveBehavior
// tolerates a nil meta unconditionally for the named-behavior path; see its
// own doc comment) — pinned here with an explicit hydrateErr (rather than
// that test's implicit "meta not loaded" stub error) so the fallback
// chain's terminal case is exercised independently of which GetWithWorkspace
// error triggered it.
func TestTaskAppServiceCreateTask_HydrationErrorAndNoBareMeta_NilMetaContinues(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{
			meta:       nil,
			hydrateErr: errors.New("workspace store unavailable"),
		},
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-unknown",
		Title:     "no meta anywhere",
		Behavior:  "any-behavior",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil", err)
	}
	if task == nil || task.Behavior != "any-behavior" {
		t.Fatalf("task = %+v, want Behavior=any-behavior", task)
	}
}
