package api

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

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

	task, err := svc.CreateTask(context.Background(), CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "workspace-only behavior",
		Behavior:  "extra",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (workspace-hydrated meta should have supplied the \"extra\" behavior)", err)
	}
	if task == nil || task.Exec == nil || task.Exec.Behavior != "extra" {
		t.Fatalf("task = %+v, want Behavior=extra", task)
	}
}

func TestTaskAppServiceCreateTask_HydrationErrorRejectsCreation(t *testing.T) {
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

	_, err := svc.CreateTask(context.Background(), CreateTaskRequest{ProjectID: "proj-1", Title: "test", Behavior: "dev"})
	if err == nil {
		t.Fatal("workspace load failure must reject task creation")
	}

}

// TestTaskAppServiceCreateTask_HydrationErrorWithoutCachedMetaRejectsCreation
// is the corner where BOTH lookups fail: workspace hydration errors AND the
// project has no cached meta at all. This must land exactly where
// TestTaskAppServiceCreateTask_ProjectNotInMeta_Skips (service_test.go)
// already lands — nil meta, task creation still succeeds (ResolveBehavior
// tolerates a nil meta unconditionally for the named-behavior path; see its
// own doc comment) — pinned here with an explicit hydrateErr (rather than
// that test's implicit "meta not loaded" stub error) so the fallback
// chain's terminal case is exercised independently of which GetWithWorkspace
// error triggered it.
func TestTaskAppServiceCreateTask_HydrationErrorWithoutCachedMetaRejectsCreation(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{
			meta:       nil,
			hydrateErr: errors.New("workspace store unavailable"),
		},
	}

	_, err := svc.CreateTask(context.Background(), CreateTaskRequest{ProjectID: "proj-1", Title: "test", Behavior: "dev"})
	if err == nil {
		t.Fatal("workspace load failure must reject task creation")
	}

}

func TestCreateTaskEmptyBranchExecutorDefersToAgent(t *testing.T) {
	svc := &TaskAppService{Tasks: &stubTaskStore{}, Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"executor": {}}}}, Projects: &stubProjectLookup{project: &orchestrator.Project{ID: "p", WorkDir: "/missing-mirror"}}}
	task, err := svc.CreateTask(context.Background(), CreateTaskRequest{ProjectID: "p", Title: "default checkout", Behavior: "executor"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Exec.BaseBranch != "" {
		t.Fatalf("branch %q, want origin default at checkout", task.Exec.BaseBranch)
	}
}
