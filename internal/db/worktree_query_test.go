package db_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/testutil"
)

func TestWorktreeCRUD(t *testing.T) {
	d := testutil.NewTestDB(t)

	// Create project and task first (FK constraints)
	d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', '/tmp/proj')`)
	d.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-1', 'proj-1', 'test', 'dev')`)

	w := &dispatcher.Worktree{
		TaskID:     "task-1",
		ProjectID:  "proj-1",
		Path:       "/home/user/.local/share/boid/worktrees/proj-1/task-1ab",
		Branch:     "boid/task-1ab",
		BaseBranch: "main",
	}

	// Create
	if err := dispatcher.CreateWorktree(d.Conn, w); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if w.ID == "" {
		t.Fatal("expected ID to be set")
	}
	if w.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}

	// Get by task
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

	// List active
	active, err := dispatcher.ListActiveWorktrees(d.Conn)
	if err != nil {
		t.Fatalf("ListActiveWorktrees: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active: got %d, want 1", len(active))
	}

	// Mark cleaned
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

	// List active should be empty now
	active, err = dispatcher.ListActiveWorktrees(d.Conn)
	if err != nil {
		t.Fatalf("ListActiveWorktrees: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active after clean: got %d, want 0", len(active))
	}

	// Double clean should error
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
