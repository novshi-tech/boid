package orchestrator_test

import (
	"encoding/json"
	"errors"
	"os"
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

// TestSetProjectWorkspace_RejectsInvalidSlug verifies the final-defense
// validation at the domain layer: even if upstream layers forget to validate,
// the DB INSERT must never run with a malformed workspace slug. See the
// 3-layer defense in docs/plans/kit-workspace-project-reorg.md.
func TestSetProjectWorkspace_RejectsInvalidSlug(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, invalid := range []string{"UPPER", "with_underscore", "with space", "..", "with/slash", strings.Repeat("a", 65)} {
		if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", invalid); err == nil {
			t.Errorf("expected SetProjectWorkspace to reject invalid slug %q", invalid)
		}
	}

	// The rejected calls must not have leaked a row into project_workspaces.
	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "" {
		t.Fatalf("invalid slug leaked into DB: workspace_id = %q", project.WorkspaceID)
	}
}

// --- AssignWorkspaceIfExists (MAJOR 3, codex review: assign+cache race
// serialization, docs/plans/workspace-db-consolidation.md) ---

// TestAssignWorkspaceIfExists_Success verifies the happy path: an existing
// workspace slug is assigned normally, exactly like SetProjectWorkspace.
func TestAssignWorkspaceIfExists_Success(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("save workspace: %v", err)
	}

	if err := orchestrator.AssignWorkspaceIfExists(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("AssignWorkspaceIfExists: %v", err)
	}

	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "team-a" {
		t.Errorf("workspace_id = %q, want team-a", project.WorkspaceID)
	}
}

// TestAssignWorkspaceIfExists_RejectsNonexistentSlug pins the atomic
// exists-check+assign contract: assigning to a slug with no workspaces row
// must fail with os.ErrNotExist wrapped and must NOT create a dangling
// project_workspaces reference — this is the single-transaction replacement
// for the old (separately racy) WorkspaceExists+SetProjectWorkspace pair.
func TestAssignWorkspaceIfExists_RejectsNonexistentSlug(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	err := orchestrator.AssignWorkspaceIfExists(d.Conn, "proj-1", "ghost")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist wrapped", err)
	}

	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "" {
		t.Errorf("dangling reference leaked: workspace_id = %q, want empty", project.WorkspaceID)
	}
}

// TestAssignWorkspaceIfExists_AcceptsDefaultSlugUnconditionally mirrors
// SetProjectWorkspace's exemption for DefaultWorkspaceSlug (it always
// exists — WorkspaceRepository.EnsureDefault guarantees this), so
// AssignWorkspaceIfExists must not require a row lookup for it either.
func TestAssignWorkspaceIfExists_AcceptsDefaultSlugUnconditionally(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.AssignWorkspaceIfExists(d.Conn, "proj-1", orchestrator.DefaultWorkspaceSlug); err != nil {
		t.Fatalf("AssignWorkspaceIfExists(default): %v", err)
	}
	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("workspace_id = %q, want %q", project.WorkspaceID, orchestrator.DefaultWorkspaceSlug)
	}
}

// TestAssignWorkspaceIfExists_EmptyClears mirrors SetProjectWorkspace's
// clear-without-existence-check behavior for workspaceID == "".
func TestAssignWorkspaceIfExists_EmptyClears(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	if err := orchestrator.AssignWorkspaceIfExists(d.Conn, "proj-1", "team-a"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := orchestrator.AssignWorkspaceIfExists(d.Conn, "proj-1", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	project, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.WorkspaceID != "" {
		t.Errorf("workspace_id = %q, want empty after clear", project.WorkspaceID)
	}
}

// TestListWorkspaces pins ListWorkspaces' PR4 rewrite
// (docs/plans/workspace-db-consolidation.md Step B): the query is now
// workspaces-table-based (LEFT JOIN project_workspaces), so a workspace with
// zero assigned projects still appears in the result — unlike the pre-PR4
// project_workspaces-GROUP-BY query, which could only ever see slugs that at
// least one project referenced.
func TestListWorkspaces(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	for _, slug := range []string{"ws-1", "ws-2", "ws-empty"} {
		if err := repo.Save(slug, &orchestrator.WorkspaceMeta{}); err != nil {
			t.Fatalf("save workspace %s: %v", slug, err)
		}
	}
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
	if len(workspaces) != 3 {
		t.Fatalf("expected 3 workspaces (including the empty one), got %d: %+v", len(workspaces), workspaces)
	}
	if workspaces[0].ID != "ws-1" || workspaces[0].ProjectCount != 2 {
		t.Fatalf("unexpected workspace 0: %+v", workspaces[0])
	}
	if workspaces[0].Revision == "" {
		t.Error("expected non-empty Revision for ws-1")
	}
	if workspaces[1].ID != "ws-2" || workspaces[1].ProjectCount != 1 {
		t.Fatalf("unexpected workspace 1: %+v", workspaces[1])
	}
	if workspaces[2].ID != "ws-empty" || workspaces[2].ProjectCount != 0 {
		t.Fatalf("unexpected workspace 2 (expected empty workspace to be listed): %+v", workspaces[2])
	}
}

// TestDeleteProject verifies that deleting a project clears its
// project_workspaces membership row. Since PR4's ListWorkspaces rewrite is
// workspaces-table-based (Step B), the workspace itself (ws-1, a real row)
// keeps appearing in ListWorkspaces after the last assigned project is
// deleted — only its ProjectCount drops to zero. This is the intended
// "empty workspaces are listed too" behavior, not a regression.
// TestGetWorkspaceSummary pins the single-slug summary lookup used by the
// workspace API handlers (docs/plans/workspace-db-consolidation.md Step
// C/D/E — building the response for create/show/update).
func TestGetWorkspaceSummary(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("ws-1", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-1", "ws-1"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}

	summary, err := orchestrator.GetWorkspaceSummary(d.Conn, "ws-1")
	if err != nil {
		t.Fatalf("GetWorkspaceSummary: %v", err)
	}
	if summary.ID != "ws-1" || summary.ProjectCount != 1 {
		t.Errorf("unexpected summary: %+v", summary)
	}
	if summary.Revision == "" {
		t.Error("expected non-empty Revision")
	}
}

// TestGetWorkspaceSummary_NotFound verifies the os.ErrNotExist contract for a
// slug with no workspaces row.
func TestGetWorkspaceSummary_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.GetWorkspaceSummary(d.Conn, "nonexistent")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want os.ErrNotExist wrapped", err)
	}
}

func TestDeleteProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := repo.Save("ws-1", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
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
	if len(workspaces) != 1 || workspaces[0].ID != "ws-1" || workspaces[0].ProjectCount != 0 {
		t.Fatalf("expected ws-1 to still be listed with ProjectCount=0, got %+v", workspaces)
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

// TestDeleteProject_WithOrphanJobs verifies that DeleteProject cleans up jobs
// that reference the project but have no task_id (sessions / standalone hooks
// not tied to a task). Without this, the jobs.project_id FOREIGN KEY refuses
// the project delete, and the daemon's auto-prune of a stale (project.yaml
// missing) DB row falls back to a startup failure on the next `boid start`.
func TestDeleteProject_WithOrphanJobs(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// task_id NULL の job (session / standalone hook) を直接 INSERT。
	// dispatcher の API を引き込まずに済むよう生 SQL で組み立てる。
	if _, err := d.Conn.Exec(
		`INSERT INTO jobs (id, task_id, project_id, status) VALUES (?, NULL, ?, 'completed')`,
		"orphan-job-1", "proj-1",
	); err != nil {
		t.Fatalf("insert orphan job: %v", err)
	}

	if err := orchestrator.DeleteProject(d.Conn, "proj-1"); err != nil {
		t.Fatalf("delete project with orphan job: %v", err)
	}

	if _, err := orchestrator.GetProject(d.Conn, "proj-1"); err == nil {
		t.Fatal("expected project to be deleted")
	}

	var remaining int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ?`, "proj-1").Scan(&remaining); err != nil {
		t.Fatalf("count remaining jobs: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected orphan jobs to be cleaned up, got %d", remaining)
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
		ProjectID: "proj-1",
		Title:     "Remote Task",
		Behavior:  "dev",
		RemoteID:  "PROJ-1",
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-1")
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
}

func TestFindTaskByRemote_NotFound(t *testing.T) {
	d := createTestProject(t)

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-NONE")
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
	if got.BranchPrefix != "fix/" {
		t.Fatalf("BranchPrefix = %q, want %q", got.BranchPrefix, "fix/")
	}
	if got.BaseBranch != "develop" {
		t.Fatalf("BaseBranch = %q, want %q", got.BaseBranch, "develop")
	}
}

func TestUpdateTask_ParentID(t *testing.T) {
	d := createTestProject(t)

	parent1 := &orchestrator.Task{ProjectID: "proj-1", Title: "P1", Behavior: "dev"}
	parent2 := &orchestrator.Task{ProjectID: "proj-1", Title: "P2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, parent1); err != nil {
		t.Fatalf("create parent1: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, parent2); err != nil {
		t.Fatalf("create parent2: %v", err)
	}

	child := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Child",
		Behavior:  "dev",
		ParentID:  parent1.ID,
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	child.ParentID = parent2.ID
	if err := orchestrator.UpdateTask(d.Conn, child); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, child.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ParentID != parent2.ID {
		t.Fatalf("ParentID = %q, want %q", got.ParentID, parent2.ID)
	}
}

func TestFindTaskByRemote_MatchesRemoteID(t *testing.T) {
	d := createTestProject(t)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev", RemoteID: "PROJ-1"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-1")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRemote() = nil, want task")
	}
	if got.ID != task1.ID {
		t.Fatalf("ID = %q, want %q", got.ID, task1.ID)
	}

	got2, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-2")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got2 != nil {
		t.Fatalf("FindTaskByRemote() = %+v, want nil", got2)
	}
}

func TestFindTaskByRemote_MultipleMatches_ReturnsLatest(t *testing.T) {
	d := createTestProject(t)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "T1", Behavior: "dev", RemoteID: "PROJ-42"}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "T2", Behavior: "dev", RemoteID: "PROJ-42"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2 (duplicate remote): %v", err)
	}

	got, err := orchestrator.FindTaskByRemote(d.Conn, "PROJ-42")
	if err != nil {
		t.Fatalf("FindTaskByRemote() error = %v", err)
	}
	if got == nil {
		t.Fatal("FindTaskByRemote() = nil, want task")
	}
	if got.ID != task2.ID {
		t.Fatalf("ID = %q, want latest %q", got.ID, task2.ID)
	}
}


