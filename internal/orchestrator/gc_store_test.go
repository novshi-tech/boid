package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestGCTasks_DeletesDoneAndAborted(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create done task: %v", err)
	}

	abortedTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Aborted Task", Behavior: "dev", Status: orchestrator.TaskStatusAborted}
	if err := orchestrator.CreateTask(d.Conn, abortedTask); err != nil {
		t.Fatalf("create aborted task: %v", err)
	}

	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create pending task: %v", err)
	}

	// アクションを作成（done タスクに紐付く）
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: doneTask.ID, Type: "start"}); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: doneTask.ID, Type: "done"}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)
	result, err := gcStore.GC(0, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 2 {
		t.Fatalf("expected 2 deleted tasks, got %d", result.Tasks)
	}
	if result.Actions != 2 {
		t.Fatalf("expected 2 deleted actions, got %d", result.Actions)
	}

	// pending タスクは残っている
	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != pendingTask.ID {
		t.Fatalf("expected pending task to remain, got %s", tasks[0].ID)
	}

	// done タスクのアクションが削除されている
	actions, err := orchestrator.ListActionsByTask(d.Conn, doneTask.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions after GC, got %d", len(actions))
	}
}

func TestGCTasks_EmptyStatuses(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// GCTasks を直接呼び出して空 statuses を確認
	result, err := orchestrator.GCTasks(d.Conn, []string{}, 0, false)
	if err != nil {
		t.Fatalf("gc tasks: %v", err)
	}
	if result.Tasks != 0 {
		t.Fatalf("expected 0 deleted tasks for empty statuses, got %d", result.Tasks)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected task to remain, got %d", len(tasks))
	}
}

func TestGCTasks_OlderThanFilter(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	oldTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Old Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, oldTask); err != nil {
		t.Fatalf("create old task: %v", err)
	}
	recentTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Recent Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, recentTask); err != nil {
		t.Fatalf("create recent task: %v", err)
	}

	// old タスクの updated_at を 60 日前に設定
	sixtyDaysAgo := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := d.Conn.Exec(
		`UPDATE tasks SET updated_at = ? WHERE id = ?`,
		sixtyDaysAgo, oldTask.ID,
	); err != nil {
		t.Fatalf("update updated_at: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)

	// dry-run: 30日以上経過したものが1件あるはず
	result, err := gcStore.GC(30*24*time.Hour, true)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("dry-run: expected 1, got %d", result.Tasks)
	}

	// タスクはまだ存在する（dry-run なので削除されていない）
	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("after dry-run: expected 2 tasks, got %d", len(tasks))
	}

	// 実際に削除
	result, err = gcStore.GC(30*24*time.Hour, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task, got %d", result.Tasks)
	}

	// recent タスクは残っている
	tasks, err = orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != recentTask.ID {
		t.Fatalf("expected recent task to remain")
	}
}

func TestGCTasks_NothingToDelete(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)
	result, err := gcStore.GC(0, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 0 {
		t.Fatalf("expected 0 deleted tasks, got %d", result.Tasks)
	}
}
