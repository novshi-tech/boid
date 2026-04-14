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
		{ProjectID: "proj-ws1-a", Title: "Dev Task 1", Behavior: "dev"},
		{ProjectID: "proj-ws1-a", Title: "Dev Task 2", Behavior: "dev"},
		{ProjectID: "proj-ws1-a", Title: "Review Task", Behavior: "review"},
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
		if task.Behavior != "dev" {
			t.Errorf("unexpected behavior %q, want dev", task.Behavior)
		}
	}
}

func TestListTasks_FilterByWorkspaceID(t *testing.T) {
	d := setupFilterTestDB(t)

	// ws-1 tasks (two projects)
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws1-a", Title: "WS1-A Task", Behavior: "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws1-b", Title: "WS1-B Task", Behavior: "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// ws-2 task
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{
		ProjectID: "proj-ws2", Title: "WS2 Task", Behavior: "dev",
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

func TestListTasks_HasDependsOn(t *testing.T) {
	d := setupFilterTestDB(t)

	standalone := &orchestrator.Task{ProjectID: "proj-ws1-a", Title: "Standalone", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, standalone); err != nil {
		t.Fatalf("create standalone: %v", err)
	}

	dep := &orchestrator.Task{ProjectID: "proj-ws1-a", Title: "Dep Source", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, dep); err != nil {
		t.Fatalf("create dep source: %v", err)
	}

	dependent := &orchestrator.Task{
		ProjectID: "proj-ws1-a",
		Title:     "Dependent",
		Behavior:  "dev",
		DependsOn: []string{dep.ID},
	}
	if err := orchestrator.CreateTask(d.Conn, dependent); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{HasDependsOn: true})
	if err != nil {
		t.Fatalf("ListTasks(has_depends_on): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ListTasks(has_depends_on): got %d tasks, want 1", len(got))
	}
	if len(got) > 0 && got[0].Title != "Dependent" {
		t.Errorf("ListTasks(has_depends_on): got %q, want Dependent", got[0].Title)
	}
}

func TestListTasks_NoDependsOn(t *testing.T) {
	d := setupFilterTestDB(t)

	standalone := &orchestrator.Task{ProjectID: "proj-ws1-a", Title: "Standalone", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, standalone); err != nil {
		t.Fatalf("create standalone: %v", err)
	}

	dep := &orchestrator.Task{ProjectID: "proj-ws1-a", Title: "Dep Source", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, dep); err != nil {
		t.Fatalf("create dep source: %v", err)
	}

	dependent := &orchestrator.Task{
		ProjectID: "proj-ws1-a",
		Title:     "Dependent",
		Behavior:  "dev",
		DependsOn: []string{dep.ID},
	}
	if err := orchestrator.CreateTask(d.Conn, dependent); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	got, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{NoDependsOn: true})
	if err != nil {
		t.Fatalf("ListTasks(no_depends_on): %v", err)
	}
	// Standalone + Dep Source = 2 tasks without depends_on
	if len(got) != 2 {
		t.Errorf("ListTasks(no_depends_on): got %d tasks, want 2", len(got))
	}
	for _, task := range got {
		if len(task.DependsOn) > 0 {
			t.Errorf("task %q should have no depends_on", task.Title)
		}
	}
}
