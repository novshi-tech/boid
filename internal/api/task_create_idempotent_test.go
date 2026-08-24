package api

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestCreateTask_GetOrCreate_HitSkipsAutoStart verifies that when CreateTask
// performs a get-or-create and the existing task is already in a non-pending
// state, auto_start does NOT fire again (no duplicate start action).
func TestCreateTask_GetOrCreate_HitSkipsAutoStart(t *testing.T) {
	existing := &orchestrator.Task{
		ID:       "existing-1",
		Ref:      "step-a",
		ParentID: "parent-1",
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
	}
	store := &stubTaskStore{
		task: existing,
		// FindTaskByRef returns the existing task when ref matches.
		refTasks: map[string]*orchestrator.Task{
			"step-a:parent-1": existing,
		},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Workflow: workflow,
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "Step A (second call)",
		Behavior:  "dev",
		Ref:       "step-a",
		ParentID:  "parent-1",
		AutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("CreateTask() returned nil task")
	}
	// Returned task must be the existing one (get-or-create hit).
	if got.ID != "existing-1" {
		t.Errorf("task ID = %q, want existing-1 (get-or-create should return existing)", got.ID)
	}
	// auto_start must NOT have fired on the already-executing task.
	if workflow.appliedType != "" {
		t.Errorf("ApplyAction called with %q, want empty (must not re-fire auto_start for non-pending task)", workflow.appliedType)
	}
}

// TestCreateTask_GetOrCreate_RootTask_HitReturnsExisting pins Phase 1 PR-4
// 論点7: the get-or-create dedup now also covers root tasks (ParentID ==
// ""), which is what makes an ingestion push idempotent — a resend of
// `task_create --initial-status parked --ref BGO-214` (no parent) must
// return the existing card task, not create a duplicate.
func TestCreateTask_GetOrCreate_RootTask_HitReturnsExisting(t *testing.T) {
	existing := &orchestrator.Task{
		ID:       "existing-card-1",
		Ref:      "BGO-214",
		ParentID: "",
		Status:   orchestrator.TaskStatusParked,
		Behavior: "dev",
	}
	store := &stubTaskStore{
		task: existing,
		refTasks: map[string]*orchestrator.Task{
			"BGO-214:": existing,
		},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Workflow: workflow,
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "BGO-214 (resend)",
		Behavior:      "dev",
		Ref:           "BGO-214",
		InitialStatus: "parked",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if got == nil || got.ID != "existing-card-1" {
		t.Fatalf("CreateTask() = %+v, want existing-card-1 (get-or-create should return existing root task)", got)
	}
}

// TestCreateTask_AutoStart_PendingTaskStarts verifies that auto_start DOES fire
// for a freshly created pending task (the normal happy path).
func TestCreateTask_AutoStart_PendingTaskStarts(t *testing.T) {
	store := &stubTaskStore{
		// FindTaskByRef returns nil → no existing task → fresh create.
		refTasks: map[string]*orchestrator.Task{},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		Workflow: workflow,
	}

	got, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "Step B",
		Behavior:  "dev",
		Ref:       "step-b",
		ParentID:  "parent-1",
		AutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("CreateTask() returned nil task")
	}
	// auto_start MUST fire for a new pending task.
	if workflow.appliedType != "start" {
		t.Errorf("ApplyAction called with %q, want start (auto_start must fire for new pending task)", workflow.appliedType)
	}
}
