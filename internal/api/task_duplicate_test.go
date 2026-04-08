package api_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestDuplicateTask_CreatesNewTask(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-proj", "Dup Project")

	// ソースタスクを作成
	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id":  "dup-proj",
		"title":       "Source Task",
		"description": "Source description",
		"behavior":    "planning",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	// 複製
	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task: %v", err)
	}

	if dup.ID == "" {
		t.Fatal("duplicated task ID should not be empty")
	}
	if dup.ID == source.ID {
		t.Error("duplicated task should have a different ID")
	}
	if dup.Title != source.Title {
		t.Errorf("Title = %q, want %q", dup.Title, source.Title)
	}
	if dup.Description != source.Description {
		t.Errorf("Description = %q, want %q", dup.Description, source.Description)
	}
	if dup.ProjectID != source.ProjectID {
		t.Errorf("ProjectID = %q, want %q", dup.ProjectID, source.ProjectID)
	}
	if dup.Behavior != source.Behavior {
		t.Errorf("Behavior = %q, want %q", dup.Behavior, source.Behavior)
	}
	if dup.Status != orchestrator.TaskStatusPending {
		t.Errorf("Status = %q, want %q", dup.Status, orchestrator.TaskStatusPending)
	}
}

func TestDuplicateTask_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	if err := ts.Client.Do("POST", "/api/tasks/nonexistent/duplicate", map[string]any{
		"auto_start": false,
	}, nil); err == nil {
		t.Fatal("expected error for nonexistent task, got nil")
	}
}

func TestDuplicateTask_VerifyListed(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProjectWithBehavior(t, ts, "dup-list-proj", "Dup List Project")

	var source orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "dup-list-proj",
		"title":      "Original",
		"behavior":   "planning",
	}, &source); err != nil {
		t.Fatalf("create source task: %v", err)
	}

	var dup orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks/"+source.ID+"/duplicate", map[string]any{
		"auto_start": false,
	}, &dup); err != nil {
		t.Fatalf("duplicate task: %v", err)
	}

	// 一覧に両タスクが存在すること
	var tasks []orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks", nil, &tasks); err != nil {
		t.Fatalf("list tasks: %v", err)
	}

	ids := make(map[string]bool)
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[source.ID] {
		t.Errorf("source task %s not found in list", source.ID)
	}
	if !ids[dup.ID] {
		t.Errorf("duplicated task %s not found in list", dup.ID)
	}
}
