package dispatcher_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestMarkStaleExecutingTasksAborted_TransitionsExecuting(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	exec1 := &orchestrator.Task{ProjectID: "proj-1", Title: "exec1", Behavior: "executor"}
	exec2 := &orchestrator.Task{ProjectID: "proj-1", Title: "exec2", Behavior: "executor"}
	for _, task := range []*orchestrator.Task{exec1, exec2} {
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task: %v", err)
		}
		if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = ?`, task.ID); err != nil {
			t.Fatalf("set executing: %v", err)
		}
	}

	n, err := dispatcher.MarkStaleExecutingTasksAborted(d.Conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 tasks aborted, got %d", n)
	}

	for _, task := range []*orchestrator.Task{exec1, exec2} {
		var status string
		if err := d.Conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, task.ID).Scan(&status); err != nil {
			t.Fatalf("query status: %v", err)
		}
		if status != "aborted" {
			t.Errorf("task %s: expected status aborted, got %s", task.ID, status)
		}
	}
}

func TestMarkStaleExecutingTasksAborted_RecordsAbortAction(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("set executing: %v", err)
	}

	if _, err := dispatcher.MarkStaleExecutingTasksAborted(d.Conn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	a := actions[0]
	if a.Type != "abort" {
		t.Errorf("expected action type abort, got %s", a.Type)
	}
	if string(a.FromStatus) != "executing" {
		t.Errorf("expected from_status executing, got %s", a.FromStatus)
	}
	if string(a.ToStatus) != "aborted" {
		t.Errorf("expected to_status aborted, got %s", a.ToStatus)
	}

	var payload map[string]string
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["code"] != "daemon_restart" {
		t.Errorf("expected code daemon_restart, got %s", payload["code"])
	}
	if payload["message"] == "" {
		t.Error("expected non-empty message in abort payload")
	}
}

func TestMarkStaleExecutingTasksAborted_SkipsNonExecuting(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	statuses := []string{"pending", "done", "aborted"}
	var tasks []*orchestrator.Task
	for _, s := range statuses {
		task := &orchestrator.Task{ProjectID: "proj-1", Title: s, Behavior: "executor"}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task: %v", err)
		}
		if s != "pending" {
			if _, err := d.Conn.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, s, task.ID); err != nil {
				t.Fatalf("set status %s: %v", s, err)
			}
		}
		tasks = append(tasks, task)
	}

	n, err := dispatcher.MarkStaleExecutingTasksAborted(d.Conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 tasks aborted, got %d", n)
	}

	for i, task := range tasks {
		var status string
		if err := d.Conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, task.ID).Scan(&status); err != nil {
			t.Fatalf("query status: %v", err)
		}
		if status != statuses[i] {
			t.Errorf("task %s: expected status %s unchanged, got %s", task.ID, statuses[i], status)
		}
	}
}

func TestMarkStaleExecutingTasksAborted_Idempotent(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("set executing: %v", err)
	}

	if _, err := dispatcher.MarkStaleExecutingTasksAborted(d.Conn); err != nil {
		t.Fatalf("first call error: %v", err)
	}
	n, err := dispatcher.MarkStaleExecutingTasksAborted(d.Conn)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 on second call (no more executing tasks), got %d", n)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected exactly 1 abort action, got %d", len(actions))
	}
}
