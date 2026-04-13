package orchestrator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func createTestProject(t *testing.T) *db.DB {
	t.Helper()
	d := testutil.NewTestDB(t)
	p := &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return d
}

func TestCreateProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	p := &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj1"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
	if p.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}
}

func TestGetProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	p := &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj1"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ID != "proj-1" {
		t.Fatalf("expected id proj-1, got %s", got.ID)
	}
	if got.WorkDir != "/tmp/proj1" {
		t.Fatalf("expected work_dir /tmp/proj1, got %s", got.WorkDir)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.GetProject(d.Conn, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListProjects(t *testing.T) {
	d := testutil.NewTestDB(t)

	projects, err := orchestrator.ListProjects(d.Conn)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("expected 0 projects, got %d", len(projects))
	}

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/b"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	projects, err = orchestrator.ListProjects(d.Conn)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
}

func TestSetProjectWorkspace(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}

	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "ws-1" {
		t.Fatalf("workspace_id = %q, want %q", project.WorkspaceID, "ws-1")
	}

	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-2"); err != nil {
		t.Fatalf("update workspace: %v", err)
	}
	project, err = orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "ws-2" {
		t.Fatalf("workspace_id = %q, want %q", project.WorkspaceID, "ws-2")
	}

	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", ""); err != nil {
		t.Fatalf("clear workspace: %v", err)
	}
	project, err = orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "" {
		t.Fatalf("workspace_id = %q, want empty", project.WorkspaceID)
	}
}

func TestListWorkspaces(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, project := range []*orchestrator.Project{
		{ID: "proj-1", WorkDir: "/tmp/a"},
		{ID: "proj-2", WorkDir: "/tmp/b"},
		{ID: "proj-3", WorkDir: "/tmp/c"},
	} {
		if err := orchestrator.CreateProject(d.Conn, project); err != nil {
			t.Fatalf("create project %s: %v", project.ID, err)
		}
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-2", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-3", "ws-2"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}

	workspaces, err := orchestrator.ListWorkspaces(d.Conn)
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(workspaces))
	}
	if workspaces[0].ID != "ws-1" || workspaces[0].ProjectCount != 2 {
		t.Fatalf("unexpected workspace 0: %+v", workspaces[0])
	}
	if workspaces[1].ID != "ws-2" || workspaces[1].ProjectCount != 1 {
		t.Fatalf("unexpected workspace 1: %+v", workspaces[1])
	}
}

func TestDeleteProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}

	if err := orchestrator.DeleteProject(d.Conn, "proj-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err == nil {
		t.Fatal("expected not found after delete")
	}

	workspaces, err := orchestrator.ListWorkspaces(d.Conn)
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("expected workspace membership to be deleted, got %+v", workspaces)
	}
}

func TestDeleteProject_WithTasks(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := orchestrator.DeleteProject(d.Conn, "proj-1"); err != nil {
		t.Fatalf("delete project with tasks: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("list tasks after project delete: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected tasks to be deleted with project, got %d", len(tasks))
	}
}

func TestDeleteProject_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.DeleteProject(d.Conn, "nonexistent")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestCreateTask(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if task.Status != orchestrator.TaskStatusPending {
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
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
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
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	prefix := task.ID[:8]
	got, err := orchestrator.GetTask(d.Conn, prefix)
	if err != nil {
		t.Fatalf("get task by prefix: %v", err)
	}
	if got.ID != task.ID {
		t.Fatalf("expected id %s, got %s", task.ID, got.ID)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.GetTask(d.Conn, "nonexistent-id-that-is-long-enough")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListTasks_NoFilter(t *testing.T) {
	d := createTestProject(t)

	for i := 0; i < 3; i++ {
		task := &orchestrator.Task{
			ProjectID: "proj-1",
			Title:     "Task",
			Behavior:  "dev",
		}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
}

func TestListTasks_WithStatusFilter(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusExecuting,
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "executing"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1, got %d", len(tasks))
	}

	tasks, err = orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{Status: "done"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected 0, got %d", len(tasks))
	}
}

func TestListTasks_WithProjectFilter(t *testing.T) {
	d := createTestProject(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-2", WorkDir: "/tmp/b"}); err != nil {
		t.Fatalf("create proj-2: %v", err)
	}

	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{ProjectID: "proj-2", Title: "B", Behavior: "dev"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{ProjectID: "proj-1"})
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
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	task.Status = orchestrator.TaskStatusExecuting
	if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("expected executing, got %s", got.Status)
	}
}

func TestCreateAction(t *testing.T) {
	d := createTestProject(t)

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
	d := createTestProject(t)

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
	d := createTestProject(t)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task1", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	for _, typ := range []string{"start", "done"} {
		if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task1.ID, Type: typ}); err != nil {
			t.Fatalf("create action: %v", err)
		}
	}
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

func TestCreateAction_WithStatusTransition(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	action := &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "start",
		FromStatus: orchestrator.TaskStatusPending,
		ToStatus:   orchestrator.TaskStatusExecuting,
	}
	if err := orchestrator.CreateAction(d.Conn, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].FromStatus != orchestrator.TaskStatusPending {
		t.Fatalf("FromStatus = %q, want %q", actions[0].FromStatus, orchestrator.TaskStatusPending)
	}
	if actions[0].ToStatus != orchestrator.TaskStatusExecuting {
		t.Fatalf("ToStatus = %q, want %q", actions[0].ToStatus, orchestrator.TaskStatusExecuting)
	}
}

func TestCreateAction_DispatchError_SameFromTo(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	action := &orchestrator.Action{
		TaskID:     task.ID,
		Type:       "dispatch_error",
		FromStatus: orchestrator.TaskStatusExecuting,
		ToStatus:   orchestrator.TaskStatusExecuting,
	}
	if err := orchestrator.CreateAction(d.Conn, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].FromStatus != orchestrator.TaskStatusExecuting {
		t.Fatalf("FromStatus = %q, want %q", actions[0].FromStatus, orchestrator.TaskStatusExecuting)
	}
	if actions[0].ToStatus != orchestrator.TaskStatusExecuting {
		t.Fatalf("ToStatus = %q, want %q", actions[0].ToStatus, orchestrator.TaskStatusExecuting)
	}
}

func TestCreateAction_LegacyNoStatusTransition(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// from/to を指定しない（既存レコード相当）
	action := &orchestrator.Action{
		TaskID: task.ID,
		Type:   "start",
	}
	if err := orchestrator.CreateAction(d.Conn, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].FromStatus != "" {
		t.Fatalf("FromStatus = %q, want empty", actions[0].FromStatus)
	}
	if actions[0].ToStatus != "" {
		t.Fatalf("ToStatus = %q, want empty", actions[0].ToStatus)
	}
}

func TestListActionsByTask_Empty(t *testing.T) {
	d := createTestProject(t)

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

func TestDeleteTask(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task to delete", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: task.ID, Type: "start"}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	if err := orchestrator.DeleteTask(d.Conn, task.ID); err != nil {
		t.Fatalf("delete task: %v", err)
	}

	_, err := orchestrator.GetTask(d.Conn, task.ID)
	if err == nil {
		t.Fatal("expected error after delete")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}

	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list actions after delete: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions after delete, got %d", len(actions))
	}
}

func TestDeleteTask_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.DeleteTask(d.Conn, "nonexistent-id-that-is-long-enough")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestFindTaskByRemote_Found(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID:    "proj-1",
		Title:        "Remote Task",
		Behavior:     "dev",
		RemoteID:     "PROJ-1",
		DataSourceID: "jira",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-1", "jira")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRemote() = nil, want task")
	}
	if got.ID != task.ID {
		t.Fatalf("ID = %q, want %q", got.ID, task.ID)
	}
	if got.RemoteID != "PROJ-1" {
		t.Fatalf("RemoteID = %q, want %q", got.RemoteID, "PROJ-1")
	}
	if got.DataSourceID != "jira" {
		t.Fatalf("DataSourceID = %q, want %q", got.DataSourceID, "jira")
	}
}

func TestFindTaskByRemote_NotFound(t *testing.T) {
	d := createTestProject(t)

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-NONE", "jira")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got != nil {
		t.Fatalf("FindTaskByRemote() = %+v, want nil", got)
	}
}

func TestCreateTask_WithBehaviorFields(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID:    "proj-1",
		Title:        "Task with behavior fields",
		Behavior:     "dev",
		Traits:       []string{"git", "sandbox"},
		Readonly:     true,
		Worktree:     true,
		BranchPrefix: "feat/",
		BaseBranch:   "main",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if len(got.Traits) != 2 || got.Traits[0] != "git" || got.Traits[1] != "sandbox" {
		t.Fatalf("Traits = %v, want [git sandbox]", got.Traits)
	}
	if !got.Readonly {
		t.Fatal("Readonly = false, want true")
	}
	if !got.Worktree {
		t.Fatal("Worktree = false, want true")
	}
	if got.BranchPrefix != "feat/" {
		t.Fatalf("BranchPrefix = %q, want %q", got.BranchPrefix, "feat/")
	}
	if got.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want %q", got.BaseBranch, "main")
	}
}

func TestCreateTask_TraitsNilRoundtrip(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task without traits",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Traits != nil {
		t.Fatalf("Traits = %v, want nil", got.Traits)
	}
	if got.Readonly {
		t.Fatal("Readonly = true, want false")
	}
	if got.Worktree {
		t.Fatal("Worktree = true, want false")
	}
}

func TestUpdateTask_BehaviorFields(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Original Title",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	task.Title = "Updated Title"
	task.Description = "Updated description"
	task.Traits = []string{"docker"}
	task.Readonly = true
	task.Worktree = true
	task.BranchPrefix = "fix/"
	task.BaseBranch = "develop"
	task.Status = orchestrator.TaskStatusExecuting
	if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Title != "Updated Title" {
		t.Fatalf("Title = %q, want %q", got.Title, "Updated Title")
	}
	if got.Description != "Updated description" {
		t.Fatalf("Description = %q, want %q", got.Description, "Updated description")
	}
	if len(got.Traits) != 1 || got.Traits[0] != "docker" {
		t.Fatalf("Traits = %v, want [docker]", got.Traits)
	}
	if !got.Readonly {
		t.Fatal("Readonly = false, want true")
	}
	if !got.Worktree {
		t.Fatal("Worktree = false, want true")
	}
	if got.BranchPrefix != "fix/" {
		t.Fatalf("BranchPrefix = %q, want %q", got.BranchPrefix, "fix/")
	}
	if got.BaseBranch != "develop" {
		t.Fatalf("BaseBranch = %q, want %q", got.BaseBranch, "develop")
	}
}

func TestFindTaskByRemote_MatchesBothFields(t *testing.T) {
	d := createTestProject(t)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev", RemoteID: "PROJ-1", DataSourceID: "jira"}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "T2", Behavior: "dev", RemoteID: "PROJ-1", DataSourceID: "github"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-1", "jira")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRemote() = nil, want task")
	}
	if got.ID != task1.ID {
		t.Fatalf("ID = %q, want %q", got.ID, task1.ID)
	}

	got2, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-1", "github")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got2 == nil {
		t.Fatal("FindTaskByRemote() = nil, want task2")
	}
	if got2.ID != task2.ID {
		t.Fatalf("ID = %q, want %q", got2.ID, task2.ID)
	}
}

func TestCreateTask_WithDependsOnPayload(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID:        "proj-1",
		Title:            "Task with depends_on_payload",
		Behavior:         "dev",
		DependsOnPayload: "branch_merged",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.DependsOnPayload != "branch_merged" {
		t.Fatalf("DependsOnPayload = %q, want %q", got.DependsOnPayload, "branch_merged")
	}
}

func TestCreateTask_WithoutDependsOnPayload(t *testing.T) {
	d := createTestProject(t)

	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Task without depends_on_payload",
		Behavior:  "dev",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.DependsOnPayload != "" {
		t.Fatalf("DependsOnPayload = %q, want empty", got.DependsOnPayload)
	}
}

