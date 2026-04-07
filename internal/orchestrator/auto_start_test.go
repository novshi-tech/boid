package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCreateTask_AutoStart_Persisted(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "auto start task",
		Behavior:  "dev",
		AutoStart: true,
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !got.AutoStart {
		t.Fatal("AutoStart should be true after round-trip")
	}
}

func TestCreateTask_AutoStart_DefaultFalse(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "normal task",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.AutoStart {
		t.Fatal("AutoStart should default to false")
	}
}

func TestListTasks_AutoStart_Persisted(t *testing.T) {
	d := createTestProject(t)

	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "auto",
		Behavior:  "dev",
		AutoStart: true,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if !tasks[0].AutoStart {
		t.Fatal("AutoStart should be true in listed task")
	}
}
