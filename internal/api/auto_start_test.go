package api

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- UpdateTask auto_start tests ---

func TestTaskAppServiceUpdateTask_AutoStart_TriggersStart(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t-1",
		Status:   orchestrator.TaskStatusPending,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Workflow: workflow,
	}

	got, err := svc.UpdateTask("t-1", UpdateTaskRequest{AutoStart: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("UpdateTask() returned nil task")
	}
	if workflow.appliedType != "start" {
		t.Fatalf("ApplyAction not called with start; appliedType = %q, want %q", workflow.appliedType, "start")
	}
}

func TestTaskAppServiceUpdateTask_AutoStart_NotPending_NoStart(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t-2",
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Workflow: workflow,
	}

	got, err := svc.UpdateTask("t-2", UpdateTaskRequest{AutoStart: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("UpdateTask() returned nil task")
	}
	if workflow.appliedType != "" {
		t.Fatalf("ApplyAction should not be called for non-pending task; appliedType = %q", workflow.appliedType)
	}
}

func TestTaskAppServiceUpdateTask_AutoStart_False_NoStart(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t-3",
		Status:   orchestrator.TaskStatusPending,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Workflow: workflow,
	}

	got, err := svc.UpdateTask("t-3", UpdateTaskRequest{AutoStart: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if got == nil {
		t.Fatal("UpdateTask() returned nil task")
	}
	if workflow.appliedType != "" {
		t.Fatalf("ApplyAction should not be called when auto_start=false; appliedType = %q", workflow.appliedType)
	}
}

func TestTaskAppServiceUpdateTask_AutoStart_NoWorkflow_NoError(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t-4",
		Status:   orchestrator.TaskStatusPending,
		Behavior: "dev",
	}
	store := &stubTaskStore{task: task}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Workflow: nil,
	}

	got, err := svc.UpdateTask("t-4", UpdateTaskRequest{AutoStart: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v when Workflow is nil, want nil", err)
	}
	if got == nil {
		t.Fatal("UpdateTask() returned nil task")
	}
}

func TestTaskAppServiceCreateTask_AutoStart_TriggersStart(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "auto task",
		Behavior:  "dev",
		AutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
	if workflow.appliedType != "start" {
		t.Fatalf("ApplyAction not called with start; appliedType = %q, want %q", workflow.appliedType, "start")
	}
}

func TestTaskAppServiceCreateTask_AutoStartFalse_StaysPending(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "normal task",
		Behavior:  "dev",
		AutoStart: false,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
	if workflow.appliedType != "" {
		t.Fatalf("ApplyAction called unexpectedly for auto_start=false; appliedType = %q", workflow.appliedType)
	}
}

func TestTaskAppServiceCreateTask_AutoStart_NoWorkflow_NoError(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{},
		Meta:     stubMetaStore{meta: meta},
		Workflow: nil,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "auto task without workflow",
		Behavior:  "dev",
		AutoStart: true,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil when Workflow is nil", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
}

func TestTaskAppServiceCreateTask_AutoStart_StartFails_TaskStillCreated(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {},
		},
	}
	store := &stubTaskStore{}
	workflow := &stubWorkflowService{
		applyActionErr: &StatusError{Code: 409, Message: "cannot start: invalid state"},
	}
	svc := &TaskAppService{
		Tasks:    store,
		Meta:     stubMetaStore{meta: meta},
		Workflow: workflow,
	}

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID: "proj-1",
		Title:     "auto task",
		Behavior:  "dev",
		AutoStart: true,
	})
	// CreateTask must succeed even if start fails (error is logged only)
	if err != nil {
		t.Fatalf("CreateTask() error = %v, want nil (task created; start error logged)", err)
	}
	if task == nil {
		t.Fatal("CreateTask() returned nil task")
	}
}
