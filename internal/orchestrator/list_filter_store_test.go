package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func setupFilterTestDB(t *testing.T) *db.DB {
	t.Helper()
	d := testutil.NewTestDB(t)
	// create two projects for workspace tests
	for _, id := range []string{"proj-ws1-a", "proj-ws1-b", "proj-ws2"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	// proj-ws1-a and proj-ws1-b belong to workspace ws-1
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-ws1-a", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-ws1-b", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	// proj-ws2 belongs to workspace ws-2
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-ws2", "ws-2"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	return d
}

func TestListTasks_FilterByBehavior(t *testing.T) {
	d := setupFilterTestDB(t)

	tasks := []*orchestrator.Task{
		{ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Dev Task 1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Dev Task 2", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}},
		{ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Review Task", Exec: &orchestrator.ExecAttrs{Behavior: "review"}},
	}
	for _, task := range tasks {
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Behavior: "dev"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListTasks(behavior=dev): got %d tasks, want 2", len(got))
	}
	for _, task := range got {
		if task.Exec.Behavior != "dev" {
			t.Errorf("unexpected behavior %q, want dev", task.Exec.Behavior)
		}
	}
}

func TestListTasks_FilterByWorkspaceID(t *testing.T) {
	d := setupFilterTestDB(t)

	// ws-1 tasks (two projects)
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "WS1-A Task", Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws1-b", Type: orchestrator.TaskTypeExecution, Title: "WS1-B Task", Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// ws-2 task
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws2", Type: orchestrator.TaskTypeExecution, Title: "WS2 Task", Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{WorkspaceID: "ws-1"})
	if err != nil {
		t.Fatalf("ListTasks(workspace=ws-1): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListTasks(workspace=ws-1): got %d tasks, want 2", len(got))
	}
	for _, task := range got {
		if task.ProjectID != "proj-ws1-a" && task.ProjectID != "proj-ws1-b" {
			t.Errorf("unexpected project %q for workspace ws-1", task.ProjectID)
		}
	}
}

func taskInResults(tasks []*orchestrator.Task, id string) bool {
	for _, t := range tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// TestListTasks_OpenTab_ExecutingParentDoneChild verifies that a done child of an executing parent
// appears in the open tab.
func TestListTasks_OpenTab_ExecutingParentDoneChild(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Parent", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ID: "child-1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", Status: orchestrator.TaskStatusDone, ParentID: "parent-1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if !taskInResults(got, "child-1") {
		t.Errorf("done child of executing parent should appear in open tab, got IDs: %v", taskIDs(got))
	}
}

// TestListTasks_OpenTab_DoneParentDoneChild verifies that a done child of a done parent
// does NOT appear in the open tab.
func TestListTasks_OpenTab_DoneParentDoneChild(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-2", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Parent", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ID: "child-2", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", Status: orchestrator.TaskStatusDone, ParentID: "parent-2", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if taskInResults(got, "child-2") {
		t.Errorf("done child of done parent should NOT appear in open tab")
	}
	if taskInResults(got, "parent-2") {
		t.Errorf("done parent with done child should NOT appear in open tab")
	}
}

// TestListTasks_OpenTab_ThreeLevels verifies that a done grandchild of an executing grandparent
// appears in the open tab (recursive ancestor check).
func TestListTasks_OpenTab_ThreeLevels(t *testing.T) {
	d := setupFilterTestDB(t)

	gp := &orchestrator.Task{ID: "gp-3", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Grandparent", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, gp); err != nil {
		t.Fatalf("create grandparent: %v", err)
	}
	mid := &orchestrator.Task{ID: "mid-3", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Middle", Status: orchestrator.TaskStatusDone, ParentID: "gp-3", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, mid); err != nil {
		t.Fatalf("create middle: %v", err)
	}
	gc := &orchestrator.Task{ID: "gc-3", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Grandchild", Status: orchestrator.TaskStatusDone, ParentID: "mid-3", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, gc); err != nil {
		t.Fatalf("create grandchild: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if !taskInResults(got, "mid-3") {
		t.Errorf("done middle child of executing grandparent should appear in open tab, got IDs: %v", taskIDs(got))
	}
	if !taskInResults(got, "gc-3") {
		t.Errorf("done grandchild of executing grandparent should appear in open tab, got IDs: %v", taskIDs(got))
	}
}

// TestListTasks_OpenTab_DoneParentExecutingChildDoneGrandchild verifies:
// - done parent with executing child is rescued by the "has open child" rule
// - done grandchild of executing child appears via ancestor rescue
func TestListTasks_OpenTab_DoneParentExecutingChildDoneGrandchild(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-4", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Parent", Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ID: "child-4", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", Status: orchestrator.TaskStatusExecuting, ParentID: "parent-4", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	gc := &orchestrator.Task{ID: "gc-4", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Grandchild", Status: orchestrator.TaskStatusDone, ParentID: "child-4", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, gc); err != nil {
		t.Fatalf("create grandchild: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if !taskInResults(got, "parent-4") {
		t.Errorf("done parent with executing child should appear in open tab (has-open-child rule), got IDs: %v", taskIDs(got))
	}
	if !taskInResults(got, "child-4") {
		t.Errorf("executing child should appear in open tab, got IDs: %v", taskIDs(got))
	}
	if !taskInResults(got, "gc-4") {
		t.Errorf("done grandchild of executing child should appear in open tab, got IDs: %v", taskIDs(got))
	}
}

// TestListTasks_OpenTab_ParkedParentDoneChild_NoLiveDescendant is a
// regression test for the "working→parked にしても done な子が Open タブに
// 残る" bug: the open_descendants ancestor-rescue CTE used to treat parked
// (a non-terminal status) the same as executing/triaged, so a done child of
// a parked parent with no other live descendants kept showing in the open
// tab forever. parked must gate the descendant-rescue clause like a terminal
// status does when nothing beneath it is actually still live.
func TestListTasks_OpenTab_ParkedParentDoneChild_NoLiveDescendant(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-5", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeCard, Title: "Parent", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ID: "child-5", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", Status: orchestrator.TaskStatusDone, ParentID: "parent-5", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if taskInResults(got, "child-5") {
		t.Errorf("done child of parked parent with no live descendant should NOT appear in open tab, got IDs: %v", taskIDs(got))
	}
	if taskInResults(got, "parent-5") {
		t.Errorf("parked parent with only a done child should NOT appear in open tab, got IDs: %v", taskIDs(got))
	}
}

// TestListTasks_OpenTab_ParkedParentDoneChildLiveGrandchild is a regression
// test for the "親がいないのに子だけ表示されている" orphan bug: a done child
// of a parked parent that itself has a still-live grandchild must not vanish
// from the open tab while its grandchild keeps showing — the whole ancestor
// chain up to the parked root must stay visible via open_ancestors so the
// tree stays consistent and progress is still trackable.
func TestListTasks_OpenTab_ParkedParentDoneChildLiveGrandchild(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-7", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeCard, Title: "Parent", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{ID: "child-7", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", Status: orchestrator.TaskStatusDone, ParentID: "parent-7", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	gc := &orchestrator.Task{ID: "gc-7", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Grandchild", Status: orchestrator.TaskStatusExecuting, ParentID: "child-7", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, gc); err != nil {
		t.Fatalf("create grandchild: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "open"})
	if err != nil {
		t.Fatalf("ListTasks(open): %v", err)
	}
	if !taskInResults(got, "gc-7") {
		t.Errorf("executing grandchild should appear in open tab, got IDs: %v", taskIDs(got))
	}
	if !taskInResults(got, "child-7") {
		t.Errorf("done child that is the direct parent of a live grandchild should appear in open tab (no orphan), got IDs: %v", taskIDs(got))
	}
	if !taskInResults(got, "parent-7") {
		t.Errorf("parked root with a live grandchild deep in its subtree should appear in open tab (no orphan), got IDs: %v", taskIDs(got))
	}
}

func taskIDs(tasks []*orchestrator.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	return ids
}

func strPtr(s string) *string { return &s }

func TestListTasks_FilterByParentID_Children(t *testing.T) {
	d := setupFilterTestDB(t)

	parent := &orchestrator.Task{ID: "parent-p1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Parent", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child1 := &orchestrator.Task{ID: "child-p1a", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child A", ParentID: "parent-p1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child1); err != nil {
		t.Fatalf("create child1: %v", err)
	}
	child2 := &orchestrator.Task{ID: "child-p1b", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child B", ParentID: "parent-p1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child2); err != nil {
		t.Fatalf("create child2: %v", err)
	}
	unrelated := &orchestrator.Task{ID: "unrelated-p1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Unrelated", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, unrelated); err != nil {
		t.Fatalf("create unrelated: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ParentID: strPtr("parent-p1")})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListTasks(parent_id=parent-p1): got %d tasks, want 2; ids=%v", len(got), taskIDs(got))
	}
	for _, task := range got {
		if task.ParentID != "parent-p1" {
			t.Errorf("unexpected parent_id %q, want parent-p1", task.ParentID)
		}
	}
}

func TestListTasks_FilterByParentID_RootOnly(t *testing.T) {
	d := setupFilterTestDB(t)

	root := &orchestrator.Task{ID: "root-r1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Root", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, root); err != nil {
		t.Fatalf("create root: %v", err)
	}
	child := &orchestrator.Task{ID: "child-r1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", ParentID: "root-r1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	// empty string selects root tasks (parent_id = "")
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ParentID: strPtr("")})
	if err != nil {
		t.Fatalf("ListTasks(parent_id=\"\"): %v", err)
	}
	if !taskInResults(got, "root-r1") {
		t.Errorf("root task should appear when parent_id=\"\", got ids=%v", taskIDs(got))
	}
	if taskInResults(got, "child-r1") {
		t.Errorf("child task should NOT appear when parent_id=\"\", got ids=%v", taskIDs(got))
	}
}

func TestListTasks_FilterByParentID_Nil_ReturnsAll(t *testing.T) {
	d := setupFilterTestDB(t)

	root := &orchestrator.Task{ID: "root-n1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Root", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, root); err != nil {
		t.Fatalf("create root: %v", err)
	}
	child := &orchestrator.Task{ID: "child-n1", ProjectID: "proj-ws1-a", Type: orchestrator.TaskTypeExecution, Title: "Child", ParentID: "root-n1", Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	// nil ParentID means no filter — both root and child appear
	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ParentID: nil})
	if err != nil {
		t.Fatalf("ListTasks(parent_id=nil): %v", err)
	}
	if !taskInResults(got, "root-n1") {
		t.Errorf("root task should appear when ParentID=nil, got ids=%v", taskIDs(got))
	}
	if !taskInResults(got, "child-n1") {
		t.Errorf("child task should appear when ParentID=nil, got ids=%v", taskIDs(got))
	}
}
