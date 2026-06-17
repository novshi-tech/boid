package api

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func boolRef(b bool) *bool { return &b }

// TestCreateTask_ExplicitReadonlyTrue_OverridesExecutorDefault verifies that
// passing Readonly: ptr(true) on an executor request overrides the behavior
// default (executor → readonly=false).
func TestCreateTask_ExplicitReadonlyTrue_OverridesExecutorDefault(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			TaskBehaviors: map[string]orchestrator.TaskBehavior{
				"supervisor": {},
				"executor":   {},
			},
		}},
	}

	v := true
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "executor with explicit readonly=true",
		Behavior:  "executor",
		Readonly:  &v,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if store.createdTask == nil {
		t.Fatal("no task was created")
	}
	if !store.createdTask.Readonly {
		t.Errorf("Readonly = false, want true (explicit override must win over executor default)")
	}
}

// TestCreateTask_ExplicitReadonlyFalse_OverridesSupervisorDefault verifies that
// passing Readonly: ptr(false) on a supervisor request overrides the behavior
// default (supervisor → readonly=true).
func TestCreateTask_ExplicitReadonlyFalse_OverridesSupervisorDefault(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			TaskBehaviors: map[string]orchestrator.TaskBehavior{
				"supervisor": {},
				"executor":   {},
			},
		}},
	}

	v := false
	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "supervisor with explicit readonly=false",
		Behavior:  "supervisor",
		Readonly:  &v,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if store.createdTask == nil {
		t.Fatal("no task was created")
	}
	if store.createdTask.Readonly {
		t.Errorf("Readonly = true, want false (explicit override must win over supervisor default)")
	}
}

// TestCreateTask_ReadonlyNil_UsesExecutorDefault verifies that omitting
// Readonly on an executor request keeps readonly=false (the behavior default).
func TestCreateTask_ReadonlyNil_UsesExecutorDefault(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			TaskBehaviors: map[string]orchestrator.TaskBehavior{
				"executor": {},
			},
		}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "executor without explicit readonly",
		Behavior:  "executor",
		Readonly:  nil,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if store.createdTask == nil {
		t.Fatal("no task was created")
	}
	if store.createdTask.Readonly {
		t.Errorf("Readonly = true, want false (executor default must be preserved when Readonly is nil)")
	}
}

// TestCreateTask_ReadonlyNil_UsesSupervisorDefault verifies that omitting
// Readonly on a supervisor request keeps readonly=true (the behavior default).
func TestCreateTask_ReadonlyNil_UsesSupervisorDefault(t *testing.T) {
	store := &stubTaskStore{}
	svc := &TaskAppService{
		Tasks: store,
		Meta: stubMetaStore{meta: &orchestrator.ProjectMeta{
			TaskBehaviors: map[string]orchestrator.TaskBehavior{
				"supervisor": {},
			},
		}},
	}

	_, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "supervisor without explicit readonly",
		Behavior:  "supervisor",
		Readonly:  nil,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if store.createdTask == nil {
		t.Fatal("no task was created")
	}
	if !store.createdTask.Readonly {
		t.Errorf("Readonly = false, want true (supervisor default must be preserved when Readonly is nil)")
	}
}
