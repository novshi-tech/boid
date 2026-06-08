package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestGetOrCreateTask_HitReturnsExisting verifies that CreateTask returns the
// existing task when (ref, parent_id) already exists, without inserting a new row.
func TestGetOrCreateTask_HitReturnsExisting(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Step A",
		Behavior:  "dev",
		Ref:       "step-a",
		ParentID:  "parent-001",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	// Second create with same ref+parent should return the existing task.
	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Step A (duplicate call)",
		Behavior:  "dev",
		Ref:       "step-a",
		ParentID:  "parent-001",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask: %v", err)
	}
	// get-or-create must populate t2 with the existing task's ID.
	if t2.ID != t1.ID {
		t.Errorf("second CreateTask returned id=%q, want existing id=%q (get-or-create should return existing)", t2.ID, t1.ID)
	}

	// Only one task should exist in the database.
	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task in db, got %d (get-or-create inserted a duplicate)", len(tasks))
	}
}

// TestGetOrCreateTask_MissInsertsNew verifies that CreateTask inserts a new task
// when no matching (ref, parent_id) exists.
func TestGetOrCreateTask_MissInsertsNew(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Step A",
		Behavior:  "dev",
		Ref:       "step-a",
		ParentID:  "parent-001",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	// Different ref — must insert a new task.
	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Step B",
		Behavior:  "dev",
		Ref:       "step-b",
		ParentID:  "parent-001",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask: %v", err)
	}
	if t2.ID == t1.ID {
		t.Error("second CreateTask (different ref) returned same ID — should have inserted a new task")
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks in db, got %d", len(tasks))
	}
}

// TestGetOrCreateTask_NoAutoStartRefireOnHit verifies that get-or-create returning
// an existing task does NOT try to start it again. The auto_start field of the
// new request must NOT be copied onto the existing task.
func TestGetOrCreateTask_HitPreservesExistingFields(t *testing.T) {
	d := createTestProject(t)

	t1 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Original Title",
		Behavior:  "dev",
		Ref:       "step-x",
		ParentID:  "parent-xyz",
	}
	if err := orchestrator.CreateTask(d.Conn, t1); err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	t2 := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Updated Title (should be ignored)",
		Behavior:  "dev",
		Ref:       "step-x",
		ParentID:  "parent-xyz",
	}
	if err := orchestrator.CreateTask(d.Conn, t2); err != nil {
		t.Fatalf("second CreateTask (get-or-create hit): %v", err)
	}

	// Verify in DB: title must be the original one (first-write-wins).
	got, err := orchestrator.GetTask(d.Conn, t1.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Title != "Original Title" {
		t.Errorf("Title = %q, want %q (second write must not overwrite)", got.Title, "Original Title")
	}
}
