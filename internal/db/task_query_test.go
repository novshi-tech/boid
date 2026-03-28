package db_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/testutil"
)

func createTestProject(t *testing.T, d *db.DB) *model.Project {
	t.Helper()
	p := &model.Project{ID: "proj-1", WorkDir: "/tmp"}
	if err := d.CreateProject(p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func TestCreateTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if task.Status != model.TaskStatusPending {
		t.Fatalf("expected default status pending, got %s", task.Status)
	}
	if string(task.Payload) != "{}" {
		t.Fatalf("expected default payload {}, got %s", string(task.Payload))
	}
	if task.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestGetTask_ByID(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := d.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.ID != task.ID {
		t.Fatalf("expected id %s, got %s", task.ID, got.ID)
	}
	if got.Title != "Test Task" {
		t.Fatalf("expected title 'Test Task', got %s", got.Title)
	}
}

func TestGetTask_ByPrefix(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Use first 8 characters as prefix
	prefix := task.ID[:8]
	got, err := d.GetTask(prefix)
	if err != nil {
		t.Fatalf("get task by prefix: %v", err)
	}
	if got.ID != task.ID {
		t.Fatalf("expected id %s, got %s", task.ID, got.ID)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := d.GetTask("nonexistent-id-that-is-long-enough")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListTasks_NoFilter(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	for i := 0; i < 3; i++ {
		task := &model.Task{
			ProjectID: "proj-1",
			Title:     "Task",
			Behavior:  "dev",
		}
		if err := d.CreateTask(task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}

	tasks, err := d.ListTasks(testutil.EmptyTaskFilter())
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
}

func TestListTasks_WithStatusFilter(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
		Status:    model.TaskStatusExecuting,
	}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create: %v", err)
	}

	tasks, err := d.ListTasks(db.TaskFilter{Status: "executing"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1, got %d", len(tasks))
	}

	tasks, err = d.ListTasks(db.TaskFilter{Status: "done"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0, got %d", len(tasks))
	}
}

func TestListTasks_WithProjectFilter(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)
	if err := d.CreateProject(&model.Project{ID: "proj-2", WorkDir: "/tmp/b"}); err != nil {
		t.Fatalf("create proj-2: %v", err)
	}

	if err := d.CreateTask(&model.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.CreateTask(&model.Task{ProjectID: "proj-2", Title: "B", Behavior: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	tasks, err := d.ListTasks(db.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1, got %d", len(tasks))
	}
	if tasks[0].ProjectID != "proj-1" {
		t.Fatalf("expected proj-1, got %s", tasks[0].ProjectID)
	}
}

func TestUpdateTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
	}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create: %v", err)
	}

	task.Status = model.TaskStatusExecuting
	if err := d.UpdateTask(task); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := d.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", got.Status)
	}
}
