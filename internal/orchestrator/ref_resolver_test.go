package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestResolvePayloadTasks_BasicCreation(t *testing.T) {
	d := createTestProject(t)

	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a"},
		{"title": "Sub B", "behavior": "dev", "ref": "b"}
	]`)

	tasks, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// Create them all
	for _, task := range tasks {
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("CreateTask(%s): %v", task.Ref, err)
		}
	}

	// Verify persistence
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 tasks in DB, got %d", len(got))
	}
}

func TestResolvePayloadTasks_RefDependsOn_ResolvedWithinBatch(t *testing.T) {
	d := createTestProject(t)
	_ = d // will be used for creating tasks after resolution

	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a"},
		{"title": "Sub B", "behavior": "dev", "ref": "b", "depends_on": ["a"]}
	]`)

	tasks, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}

	// Find tasks by ref
	var taskA, taskB *orchestrator.Task
	for _, task := range tasks {
		switch task.Ref {
		case "a":
			taskA = task
		case "b":
			taskB = task
		}
	}

	if taskA == nil || taskB == nil {
		t.Fatal("expected tasks with ref 'a' and 'b'")
	}

	// taskB.DependsOn should be resolved to taskA.ID
	if len(taskB.DependsOn) != 1 {
		t.Fatalf("taskB.DependsOn = %v, want 1 entry", taskB.DependsOn)
	}
	if taskB.DependsOn[0] != taskA.ID {
		t.Errorf("taskB.DependsOn[0] = %q, want taskA.ID %q", taskB.DependsOn[0], taskA.ID)
	}

	// taskA has no depends_on
	if len(taskA.DependsOn) != 0 {
		t.Errorf("taskA.DependsOn = %v, want empty", taskA.DependsOn)
	}
}

func TestResolvePayloadTasks_UUIDDependsOn_PassedThrough(t *testing.T) {
	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a",
		 "depends_on": ["aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"]}
	]`)

	tasks, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}

	// UUID depends_on should be passed through unchanged
	if len(tasks[0].DependsOn) != 1 {
		t.Fatalf("DependsOn = %v, want 1 entry", tasks[0].DependsOn)
	}
	if tasks[0].DependsOn[0] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("DependsOn[0] = %q, want UUID", tasks[0].DependsOn[0])
	}
}

func TestResolvePayloadTasks_UnresolvableRef_Error(t *testing.T) {
	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a",
		 "depends_on": ["nonexistent-ref"]}
	]`)

	_, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err == nil {
		t.Fatal("expected error for unresolvable ref, got nil")
	}
}

func TestResolvePayloadTasks_ParentIDSetOnAllTasks(t *testing.T) {
	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a"},
		{"title": "Sub B", "behavior": "dev"}
	]`)

	tasks, err := orchestrator.ResolvePayloadTasks("my-parent-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}
	for _, task := range tasks {
		if task.ParentID != "my-parent-id" {
			t.Errorf("task %q ParentID = %q, want %q", task.Title, task.ParentID, "my-parent-id")
		}
		if task.ProjectID != "proj-1" {
			t.Errorf("task %q ProjectID = %q, want %q", task.Title, task.ProjectID, "proj-1")
		}
	}
}

func TestResolvePayloadTasks_EmptyArray_ReturnsEmpty(t *testing.T) {
	tasksJSON := json.RawMessage(`[]`)

	tasks, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected empty, got %d tasks", len(tasks))
	}
}

func TestResolvePayloadTasks_IDPreAssigned(t *testing.T) {
	tasksJSON := json.RawMessage(`[
		{"title": "Sub A", "behavior": "dev", "ref": "a"},
		{"title": "Sub B", "behavior": "dev", "ref": "b", "depends_on": ["a"]}
	]`)

	tasks, err := orchestrator.ResolvePayloadTasks("parent-task-id", "proj-1", tasksJSON)
	if err != nil {
		t.Fatalf("ResolvePayloadTasks: %v", err)
	}

	// Each task must have a pre-assigned ID so that depends_on resolution works
	for _, task := range tasks {
		if task.ID == "" {
			t.Errorf("task %q has empty ID, want pre-assigned UUID", task.Title)
		}
	}
}
