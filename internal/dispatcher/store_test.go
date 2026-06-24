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

// After a restart no agent is parked in the in-memory BlockingAskRegistry, so
// awaiting tasks are zombies and must be reclaimed: aborted with from_status
// awaiting and the daemon_shutdown code (so the startup auto-reopen picks them
// up). Executing tasks are left to MarkStaleExecutingTasksAborted.
func TestMarkStaleAwaitingTasksAborted_TransitionsAwaiting(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	awaiting := &orchestrator.Task{ProjectID: "proj-1", Title: "awaiting", Behavior: "executor"}
	executing := &orchestrator.Task{ProjectID: "proj-1", Title: "executing", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, awaiting); err != nil {
		t.Fatalf("create awaiting: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, executing); err != nil {
		t.Fatalf("create executing: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'awaiting' WHERE id = ?`, awaiting.ID); err != nil {
		t.Fatalf("set awaiting: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = ?`, executing.ID); err != nil {
		t.Fatalf("set executing: %v", err)
	}

	n, err := dispatcher.MarkStaleAwaitingTasksAborted(d.Conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 awaiting task aborted, got %d", n)
	}

	var awaitingStatus, execStatus string
	if err := d.Conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, awaiting.ID).Scan(&awaitingStatus); err != nil {
		t.Fatalf("query awaiting status: %v", err)
	}
	if awaitingStatus != "aborted" {
		t.Errorf("awaiting task status = %s, want aborted", awaitingStatus)
	}
	if err := d.Conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, executing.ID).Scan(&execStatus); err != nil {
		t.Fatalf("query executing status: %v", err)
	}
	if execStatus != "executing" {
		t.Errorf("executing task should be untouched by the awaiting reaper, got %s", execStatus)
	}

	// The abort action carries from_status=awaiting and the daemon_shutdown code.
	var fromStatus string
	var payload []byte
	if err := d.Conn.QueryRow(
		`SELECT from_status, payload FROM actions WHERE task_id = ? AND type = 'abort'`, awaiting.ID,
	).Scan(&fromStatus, &payload); err != nil {
		t.Fatalf("query abort action: %v", err)
	}
	if fromStatus != "awaiting" {
		t.Errorf("abort from_status = %s, want awaiting", fromStatus)
	}
	var pj struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &pj); err != nil {
		t.Fatalf("unmarshal abort payload: %v", err)
	}
	if pj.Code != "daemon_shutdown" {
		t.Errorf("abort code = %s, want daemon_shutdown (for auto-reopen)", pj.Code)
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
	if payload["code"] != "daemon_shutdown" {
		t.Errorf("expected code daemon_shutdown, got %s", payload["code"])
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

// FindDaemonShutdownAbortedTasks must return tasks whose latest aborted-
// transition action has code=daemon_shutdown, and skip those aborted for
// other reasons. Used by the startup auto-reopen path.
func TestFindDaemonShutdownAbortedTasks_ReturnsShutdownTasks(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Task A: aborted via daemon_shutdown — should be returned
	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'aborted' WHERE id = ?`, taskA.ID); err != nil {
		t.Fatalf("set A aborted: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{
		TaskID:     taskA.ID,
		Type:       "abort",
		FromStatus: orchestrator.TaskStatusExecuting,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    json.RawMessage(`{"code":"daemon_shutdown","message":"shutdown"}`),
	}); err != nil {
		t.Fatalf("create A action: %v", err)
	}

	// Task B: aborted via dispatch_error — should NOT be returned
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create B: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'aborted' WHERE id = ?`, taskB.ID); err != nil {
		t.Fatalf("set B aborted: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{
		TaskID:     taskB.ID,
		Type:       "abort",
		FromStatus: orchestrator.TaskStatusExecuting,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    json.RawMessage(`{"code":"dispatch_error","message":"hook failed"}`),
	}); err != nil {
		t.Fatalf("create B action: %v", err)
	}

	// Task C: executing — should NOT be returned even if some action history
	taskC := &orchestrator.Task{ProjectID: "proj-1", Title: "C", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, taskC); err != nil {
		t.Fatalf("create C: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = ?`, taskC.ID); err != nil {
		t.Fatalf("set C executing: %v", err)
	}

	ids, err := dispatcher.FindDaemonShutdownAbortedTasks(d.Conn)
	if err != nil {
		t.Fatalf("FindDaemonShutdownAbortedTasks: %v", err)
	}
	if len(ids) != 1 || ids[0] != taskA.ID {
		t.Errorf("expected [%s], got %v", taskA.ID, ids)
	}
}

// If a task was aborted by daemon_shutdown first and then aborted again
// later for another reason (e.g. user retried, hook failed), the LATER
// abort wins and the task must NOT be auto-reopened.
func TestFindDaemonShutdownAbortedTasks_LatestAbortWins(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "task", Behavior: "executor"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'aborted' WHERE id = ?`, task.ID); err != nil {
		t.Fatalf("set aborted: %v", err)
	}

	// First abort: daemon_shutdown
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "abort",
		FromStatus: orchestrator.TaskStatusExecuting,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    json.RawMessage(`{"code":"daemon_shutdown"}`),
	}); err != nil {
		t.Fatalf("create first abort: %v", err)
	}
	// Newer abort: dispatch_error (e.g. reopen then hook failed)
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "abort",
		FromStatus: orchestrator.TaskStatusExecuting,
		ToStatus:   orchestrator.TaskStatusAborted,
		Payload:    json.RawMessage(`{"code":"dispatch_error"}`),
	}); err != nil {
		t.Fatalf("create second abort: %v", err)
	}

	ids, err := dispatcher.FindDaemonShutdownAbortedTasks(d.Conn)
	if err != nil {
		t.Fatalf("FindDaemonShutdownAbortedTasks: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected no tasks (latest abort is dispatch_error), got %v", ids)
	}
}

// Empty DB / no aborted tasks must return nil, not an error.
func TestFindDaemonShutdownAbortedTasks_Empty(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	ids, err := dispatcher.FindDaemonShutdownAbortedTasks(d.Conn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty result, got %v", ids)
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
