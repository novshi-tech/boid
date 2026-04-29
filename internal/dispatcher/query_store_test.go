package dispatcher_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func createDispatcherTask(t *testing.T) *db.DB {
	t.Helper()
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return d
}

func TestCreateJob(t *testing.T) {
	d := createDispatcherTask(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &dispatcher.Job{
		TaskID:      task.ID,
		ProjectID:   "proj-1",
		HandlerID:   "hook-1",
		RuntimeID:   "runtime-1",
		Interactive: true,
		TTY:         true,
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if job.Status != dispatcher.JobStatusRunning {
		t.Fatalf("expected default status running, got %s", job.Status)
	}
	if job.RuntimeID != "runtime-1" {
		t.Fatalf("expected runtime_id runtime-1, got %q", job.RuntimeID)
	}
	if !job.Interactive {
		t.Fatal("expected interactive to be true")
	}
	if !job.TTY {
		t.Fatal("expected tty to be true")
	}
	if job.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestGetJob(t *testing.T) {
	d := createDispatcherTask(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &dispatcher.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HandlerID: "hook-1",
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	got, err := dispatcher.GetJob(d.Conn, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.ID != job.ID {
		t.Fatalf("expected id %s, got %s", job.ID, got.ID)
	}
	if got.TaskID != task.ID {
		t.Fatalf("expected task_id %s, got %s", task.ID, got.TaskID)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("expected project_id proj-1, got %s", got.ProjectID)
	}
	if got.HandlerID != "hook-1" {
		t.Fatalf("expected handler_id hook-1, got %s", got.HandlerID)
	}
	if got.RuntimeID != "" {
		t.Fatalf("expected empty runtime_id, got %q", got.RuntimeID)
	}
	if got.Interactive {
		t.Fatal("expected interactive false")
	}
	if got.TTY {
		t.Fatal("expected tty false")
	}
	if got.Status != dispatcher.JobStatusRunning {
		t.Fatalf("expected running, got %s", got.Status)
	}
}

func TestCreateJob_NoTask(t *testing.T) {
	d := createDispatcherTask(t)

	job := &dispatcher.Job{
		ProjectID: "proj-1",
		HandlerID: "",
		Role:      "exec",
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job without task: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected auto-generated ID")
	}

	got, err := dispatcher.GetJob(d.Conn, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.TaskID != "" {
		t.Fatalf("expected empty task_id, got %q", got.TaskID)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("expected project_id proj-1, got %s", got.ProjectID)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := dispatcher.GetJob(d.Conn, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListJobsByTask(t *testing.T) {
	d := createDispatcherTask(t)

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task1", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "Task2", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := dispatcher.CreateJob(d.Conn, &dispatcher.Job{TaskID: task1.ID, ProjectID: "proj-1", HandlerID: "hook-1"}); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	if err := dispatcher.CreateJob(d.Conn, &dispatcher.Job{TaskID: task2.ID, ProjectID: "proj-1", HandlerID: "hook-1"}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	jobs, err := dispatcher.ListJobsByTask(d.Conn, task1.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs for task1, got %d", len(jobs))
	}

	jobs, err = dispatcher.ListJobsByTask(d.Conn, task2.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for task2, got %d", len(jobs))
	}
}

func TestListJobsByTask_Empty(t *testing.T) {
	d := createDispatcherTask(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	jobs, err := dispatcher.ListJobsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestUpdateJob(t *testing.T) {
	d := createDispatcherTask(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &dispatcher.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HandlerID: "hook-1",
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	job.Status = dispatcher.JobStatusCompleted
	job.ExitCode = 0
	job.Output = "success"
	job.RuntimeID = "runtime-success"
	job.Interactive = true
	job.TTY = true
	if err := dispatcher.UpdateJob(d.Conn, job); err != nil {
		t.Fatalf("update job: %v", err)
	}

	got, err := dispatcher.GetJob(d.Conn, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != dispatcher.JobStatusCompleted {
		t.Fatalf("expected completed, got %s", got.Status)
	}
	if got.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", got.ExitCode)
	}
	if got.Output != "success" {
		t.Fatalf("expected output 'success', got %s", got.Output)
	}
	if got.RuntimeID != "runtime-success" {
		t.Fatalf("expected runtime_id runtime-success, got %q", got.RuntimeID)
	}
	if !got.Interactive {
		t.Fatal("expected interactive true")
	}
	if !got.TTY {
		t.Fatal("expected tty true")
	}
}

func TestUpdateJob_Failed(t *testing.T) {
	d := createDispatcherTask(t)

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &dispatcher.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HandlerID: "hook-1",
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	job.Status = dispatcher.JobStatusFailed
	job.ExitCode = 1
	job.Output = "error occurred"
	job.RuntimeID = "runtime-failed"
	if err := dispatcher.UpdateJob(d.Conn, job); err != nil {
		t.Fatalf("update job: %v", err)
	}

	got, err := dispatcher.GetJob(d.Conn, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != dispatcher.JobStatusFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	if got.ExitCode != 1 {
		t.Fatalf("expected exit_code 1, got %d", got.ExitCode)
	}
	if got.Output != "error occurred" {
		t.Fatalf("expected output 'error occurred', got %s", got.Output)
	}
	if got.RuntimeID != "runtime-failed" {
		t.Fatalf("expected runtime_id runtime-failed, got %q", got.RuntimeID)
	}
}

func TestWorktreeCRUD(t *testing.T) {
	d := testutil.NewTestDB(t)

	d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', '/tmp/proj')`)
	d.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-1', 'proj-1', 'test', 'dev')`)

	w := &dispatcher.Worktree{
		TaskID:     "task-1",
		ProjectID:  "proj-1",
		Path:       "/home/user/.local/share/boid/worktrees/proj-1/task-1ab",
		Branch:     "boid/task-1ab",
		BaseBranch: "main",
	}

	if err := dispatcher.CreateWorktree(d.Conn, w); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if w.ID == "" {
		t.Fatal("expected ID to be set")
	}
	if w.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}

	got, err := dispatcher.GetWorktreeByTask(d.Conn, "task-1")
	if err != nil {
		t.Fatalf("GetWorktreeByTask: %v", err)
	}
	if got == nil {
		t.Fatal("expected worktree, got nil")
	}
	if got.Path != w.Path {
		t.Errorf("path: got %q, want %q", got.Path, w.Path)
	}
	if got.Branch != w.Branch {
		t.Errorf("branch: got %q, want %q", got.Branch, w.Branch)
	}
	if got.CleanedAt != nil {
		t.Error("expected CleanedAt to be nil")
	}

	active, err := dispatcher.ListActiveWorktrees(d.Conn)
	if err != nil {
		t.Fatalf("ListActiveWorktrees: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active: got %d, want 1", len(active))
	}

	if err := dispatcher.MarkWorktreeCleaned(d.Conn, "task-1"); err != nil {
		t.Fatalf("MarkWorktreeCleaned: %v", err)
	}

	got, err = dispatcher.GetWorktreeByTask(d.Conn, "task-1")
	if err != nil {
		t.Fatalf("GetWorktreeByTask after clean: %v", err)
	}
	if got.CleanedAt == nil {
		t.Error("expected CleanedAt to be set after MarkWorktreeCleaned")
	}

	active, err = dispatcher.ListActiveWorktrees(d.Conn)
	if err != nil {
		t.Fatalf("ListActiveWorktrees: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active after clean: got %d, want 0", len(active))
	}

	if err := dispatcher.MarkWorktreeCleaned(d.Conn, "task-1"); err == nil {
		t.Error("expected error on double MarkWorktreeCleaned")
	}
}

func TestGetWorktreeByTask_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)

	got, err := dispatcher.GetWorktreeByTask(d.Conn, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent task")
	}
}
