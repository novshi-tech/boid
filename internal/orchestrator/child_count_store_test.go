package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestChildCount_NoChildren(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalChildCount != 0 {
		t.Errorf("TotalChildCount = %d, want 0", got.TotalChildCount)
	}
	if got.DoneChildCount != 0 {
		t.Errorf("DoneChildCount = %d, want 0", got.DoneChildCount)
	}
	if got.AbortedChildCount != 0 {
		t.Errorf("AbortedChildCount = %d, want 0", got.AbortedChildCount)
	}
	if got.OpenChildCount != 0 {
		t.Errorf("OpenChildCount = %d, want 0", got.OpenChildCount)
	}
}

func TestChildCount_MixedChildren(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	statuses := []orchestrator.TaskStatus{
		orchestrator.TaskStatusPending,
		orchestrator.TaskStatusDone,
		orchestrator.TaskStatusAborted,
		orchestrator.TaskStatusExecuting,
	}
	for _, s := range statuses {
		child := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     "Child",
			Behavior:  "dev",
			Status:    s,
			ParentID:  parent.ID,
		}
		if err := orchestrator.CreateTask(d.Conn, child); err != nil {
			t.Fatalf("create child %s: %v", s, err)
		}
	}

	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalChildCount != 4 {
		t.Errorf("TotalChildCount = %d, want 4", got.TotalChildCount)
	}
	if got.DoneChildCount != 1 {
		t.Errorf("DoneChildCount = %d, want 1", got.DoneChildCount)
	}
	if got.AbortedChildCount != 1 {
		t.Errorf("AbortedChildCount = %d, want 1", got.AbortedChildCount)
	}
	// pending + executing = 2 open
	if got.OpenChildCount != 2 {
		t.Errorf("OpenChildCount = %d, want 2", got.OpenChildCount)
	}
}

func TestChildCount_AddChildAfterParent(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// 最初は子なし
	got, err := orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalChildCount != 0 {
		t.Errorf("initial TotalChildCount = %d, want 0", got.TotalChildCount)
	}

	// 後から子を追加
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Child",
		Behavior:  "dev",
		ParentID:  parent.ID,
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err = orchestrator.GetTask(d.Conn, parent.ID)
	if err != nil {
		t.Fatalf("get after add: %v", err)
	}
	if got.TotalChildCount != 1 {
		t.Errorf("TotalChildCount after add = %d, want 1", got.TotalChildCount)
	}
	if got.OpenChildCount != 1 {
		t.Errorf("OpenChildCount after add = %d, want 1", got.OpenChildCount)
	}
}

func TestChildCount_ReparentChild(t *testing.T) {
	d := createTestProject(t)
	parent1 := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent1", Behavior: "dev"}
	parent2 := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent1); err != nil {
		t.Fatalf("create parent1: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, parent2); err != nil {
		t.Fatalf("create parent2: %v", err)
	}

	child := &orchestrator.Task{ProjectID: "proj-1", Title: "Child", Behavior: "dev", ParentID: parent1.ID}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	// 移行前: parent1 に 1 子、parent2 に 0 子
	got1, err := orchestrator.GetTask(d.Conn, parent1.ID)
	if err != nil {
		t.Fatalf("get parent1: %v", err)
	}
	got2, err := orchestrator.GetTask(d.Conn, parent2.ID)
	if err != nil {
		t.Fatalf("get parent2: %v", err)
	}
	if got1.TotalChildCount != 1 {
		t.Errorf("parent1 TotalChildCount = %d, want 1", got1.TotalChildCount)
	}
	if got2.TotalChildCount != 0 {
		t.Errorf("parent2 TotalChildCount = %d, want 0", got2.TotalChildCount)
	}

	// 子を parent2 に移す（サブクエリ集計なので即座に反映される）
	if _, err := d.Conn.Exec(`UPDATE tasks SET parent_id = ? WHERE id = ?`, parent2.ID, child.ID); err != nil {
		t.Fatalf("reparent: %v", err)
	}

	got1, err = orchestrator.GetTask(d.Conn, parent1.ID)
	if err != nil {
		t.Fatalf("get parent1 after reparent: %v", err)
	}
	got2, err = orchestrator.GetTask(d.Conn, parent2.ID)
	if err != nil {
		t.Fatalf("get parent2 after reparent: %v", err)
	}
	if got1.TotalChildCount != 0 {
		t.Errorf("parent1 TotalChildCount after reparent = %d, want 0", got1.TotalChildCount)
	}
	if got2.TotalChildCount != 1 {
		t.Errorf("parent2 TotalChildCount after reparent = %d, want 1", got2.TotalChildCount)
	}
}

func TestListTasks_OpenFilter_ClosedParentWithOpenChild(t *testing.T) {
	d := createTestProject(t)

	// 親は done 状態
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parent",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// 子は pending（open）
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Child",
		Behavior:  "dev",
		ParentID:  parent.ID,
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	ids := make(map[string]bool)
	for _, task := range tasks {
		ids[task.ID] = true
	}
	if !ids[parent.ID] {
		t.Error("closed parent with open child should appear in open list")
	}
	if !ids[child.ID] {
		t.Error("open child should appear in open list")
	}
}

func TestListTasks_OpenFilter_ClosedParentAllChildrenDone(t *testing.T) {
	d := createTestProject(t)

	// 親は done 状態
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parent",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// 子も done 状態
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Child",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
		ParentID:  parent.ID,
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	for _, task := range tasks {
		if task.ID == parent.ID {
			t.Error("closed parent with all-done children should NOT appear in open list")
		}
		if task.ID == child.ID {
			t.Error("done child should NOT appear in open list")
		}
	}
}

func TestListTasks_OpenFilter_ClosedParentAbortedChild(t *testing.T) {
	d := createTestProject(t)

	// 親は aborted 状態
	parent := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Parent",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusAborted,
	}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// 子も aborted 状態
	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Child",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusAborted,
		ParentID:  parent.ID,
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	for _, task := range tasks {
		if task.ID == parent.ID || task.ID == child.ID {
			t.Errorf("task %s (aborted, no open children) should NOT appear in open list", task.ID)
		}
	}
}

func TestListTasks_ChildCountInList(t *testing.T) {
	d := createTestProject(t)
	parent := &orchestrator.Task{ProjectID: "proj-1", Title: "Parent", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	for i := 0; i < 3; i++ {
		child := &orchestrator.Task{ProjectID: "proj-1", Title: "Child", Behavior: "dev", ParentID: parent.ID}
		if err := orchestrator.CreateTask(d.Conn, child); err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, task := range tasks {
		if task.ID == parent.ID {
			if task.TotalChildCount != 3 {
				t.Errorf("parent TotalChildCount in list = %d, want 3", task.TotalChildCount)
			}
			if task.OpenChildCount != 3 {
				t.Errorf("parent OpenChildCount in list = %d, want 3", task.OpenChildCount)
			}
			return
		}
	}
	t.Fatal("parent not found in list")
}

func TestListTasks_OpenFilter_OpenTasksOnly(t *testing.T) {
	d := testutil.NewTestDB(t)
	p := &orchestrator.Project{ID: "proj-x", WorkDir: "/tmp"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	openTask := &orchestrator.Task{ProjectID: "proj-x", Title: "Open", Behavior: "dev", Status: orchestrator.TaskStatusPending}
	if err := orchestrator.CreateTask(d.Conn, openTask); err != nil {
		t.Fatalf("create open task: %v", err)
	}
	doneTask := &orchestrator.Task{ProjectID: "proj-x", Title: "Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create done task: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 open task, got %d", len(tasks))
	}
	if tasks[0].ID != openTask.ID {
		t.Errorf("expected openTask, got %s", tasks[0].ID)
	}
}
