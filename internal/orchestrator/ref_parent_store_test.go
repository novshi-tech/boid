package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCreateTask_RefAndParentID_Persisted(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task with ref",
		Behavior:  "dev",
		Ref:       "task-a",
		ParentID:  "parent-uuid-1234",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Ref != "task-a" {
		t.Errorf("Ref = %q, want %q", got.Ref, "task-a")
	}
	if got.ParentID != "parent-uuid-1234" {
		t.Errorf("ParentID = %q, want %q", got.ParentID, "parent-uuid-1234")
	}
}

func TestCreateTask_EmptyRef_NoUniqueConstraint(t *testing.T) {
	d := createTestProject(t)

	// Multiple tasks with empty ref should not conflict
	for i := 0; i < 3; i++ {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     "Task without ref",
			Behavior:  "dev",
			// Ref:      "" (zero value, no ref)
			ParentID: "same-parent",
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("CreateTask[%d]: %v", i, err)
		}
	}
}

// TestCreateTask_SameRefSameParent_GetOrCreate verifies that creating a second
// task with the same (ref, parent_id) returns the existing task instead of
// failing with a unique constraint error (get-or-create semantics).
func TestCreateTask_SameRefSameParent_GetOrCreate(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task A",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-abc",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task B (same ref — should get existing)",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-abc",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask with same ref: %v (want get-or-create, not error)", err)
	}
	if t2.ID != t1.ID {
		t.Errorf("second CreateTask returned id=%q, want existing id=%q", t2.ID, t1.ID)
	}
}

func TestCreateTask_SameRefDifferentParent_OK(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task A",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-aaa",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task B",
		Behavior:  "dev",
		Ref:       "step-1",
		ParentID:  "parent-uuid-bbb",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask (different parent): %v", err)
	}
}

func TestListTasks_RefAndParentID_Persisted(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task with ref",
		Behavior:  "dev",
		Ref:       "my-ref",
		ParentID:  "my-parent",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Ref != "my-ref" {
		t.Errorf("Ref = %q, want %q", tasks[0].Ref, "my-ref")
	}
	if tasks[0].ParentID != "my-parent" {
		t.Errorf("ParentID = %q, want %q", tasks[0].ParentID, "my-parent")
	}
}

func TestFindTaskByRef_Found(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ref task",
		Behavior:  "dev",
		Ref:       "step-2",
		ParentID:  "parent-xyz",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.FindTaskByRef(d.Conn, "step-2", "parent-xyz")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRef returned nil, want task")
	}
	if got.ID != task.ID {
		t.Errorf("ID = %q, want %q", got.ID, task.ID)
	}
	if got.Ref != "step-2" {
		t.Errorf("Ref = %q, want %q", got.Ref, "step-2")
	}
}

func TestFindTaskByRef_NotFound_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	got, err := orchestrator.FindTaskByRef(d.Conn, "nonexistent", "parent-xyz")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned %+v, want nil", got)
	}
}

func TestFindTaskByRef_UUID_LooksUpByID(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task without ref",
		Behavior:  "dev",
		// Ref is empty — only referenceable by UUID
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Look up by the task's UUID (not by ref name)
	got, err := orchestrator.FindTaskByRef(d.Conn, task.ID, "")
	if err != nil {
		t.Fatalf("FindTaskByRef(uuid): %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRef(uuid) returned nil, want task")
	}
	if got.ID != task.ID {
		t.Errorf("ID = %q, want %q", got.ID, task.ID)
	}
}

func TestFindTaskByRef_UUID_NotFound_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	// Non-existent UUID
	got, err := orchestrator.FindTaskByRef(d.Conn, "00000000-0000-0000-0000-000000000000", "")
	if err != nil {
		t.Fatalf("FindTaskByRef(nonexistent uuid): %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned %+v, want nil for nonexistent UUID", got)
	}
}

func TestFindTaskByRef_WrongParent_ReturnsNil(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Ref task",
		Behavior:  "dev",
		Ref:       "step-3",
		ParentID:  "parent-correct",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.FindTaskByRef(d.Conn, "step-3", "parent-wrong")
	if err != nil {
		t.Fatalf("FindTaskByRef: %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRef returned task for wrong parent, want nil")
	}
}
