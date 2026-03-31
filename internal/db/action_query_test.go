package db_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateAction(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	action := &orchestrator.Action{
		TaskID:  task.ID,
		Type:    "start",
		Payload: json.RawMessage(`{"key":"value"}`),
	}
	if err := orchestrator.CreateAction(d.Conn, action); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if action.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if action.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestCreateAction_DefaultPayload(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	action := &orchestrator.Action{
		TaskID: task.ID,
		Type:   "start",
	}
	if err := orchestrator.CreateAction(d.Conn, action); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if string(action.Payload) != "{}" {
		t.Fatalf("expected default payload {}, got %s", string(action.Payload))
	}
}

func TestListActionsByTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task1", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	// Create actions for task1
	for _, typ := range []string{"start", "done"} {
		if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task1.ID, Type: typ}); err != nil {
			t.Fatalf("create action: %v", err)
		}
	}
	// Create action for task2
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task2.ID, Type: "start"}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task1.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions for task1, got %d", len(actions))
	}
	if actions[0].Type != "start" {
		t.Fatalf("expected first action type 'start', got %s", actions[0].Type)
	}
	if actions[1].Type != "done" {
		t.Fatalf("expected second action type 'done', got %s", actions[1].Type)
	}

	actions, err = orchestrator.ListActionsByTask(d.Conn, task2.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action for task2, got %d", len(actions))
	}
}

func TestListActionsByTask_Empty(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
}
