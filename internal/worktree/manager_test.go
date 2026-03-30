package worktree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/worktree"
	"github.com/novshi-tech/boid/testutil"
)

const gitBin = "/usr/bin/git"

// initGitRepo creates a temporary git repo with an initial commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Initial commit
	f := filepath.Join(dir, "README.md")
	os.WriteFile(f, []byte("# test"), 0o644)
	exec.Command(gitBin, "-C", dir, "add", ".").Run()
	cmd := exec.Command(gitBin, "-C", dir, "commit", "-m", "initial")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

func TestManager_CreateAndRemove(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	// Register project and task in DB
	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-abcd1234-5678', 'proj-1', 'test', 'dev')`)

	mgr := &worktree.Manager{RootDir: wtRoot, DB: db, GitBin: gitBin}

	// Create
	w, err := mgr.Create(repo, "proj-1", "task-abcd1234-5678", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	expectedPath := filepath.Join(wtRoot, "proj-1", "task-abc")
	if w.Path != expectedPath {
		t.Errorf("path: got %q, want %q", w.Path, expectedPath)
	}
	if w.Branch != "boid/task-abc" {
		t.Errorf("branch: got %q, want %q", w.Branch, "boid/task-abc")
	}

	// Verify worktree directory exists
	if _, err := os.Stat(w.Path); err != nil {
		t.Errorf("worktree dir should exist: %v", err)
	}

	// Verify .git file exists (worktree has .git file, not directory)
	gitFile := filepath.Join(w.Path, ".git")
	info, err := os.Stat(gitFile)
	if err != nil {
		t.Fatalf(".git file should exist: %v", err)
	}
	if info.IsDir() {
		t.Error(".git should be a file in worktree, not a directory")
	}

	// Verify branch exists
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) == 0 {
		t.Error("branch should exist after Create")
	}

	// Get
	got, err := mgr.Get("task-abcd1234-5678")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Path != w.Path {
		t.Error("Get should return the created worktree")
	}

	// Remove (done mode — keep branch)
	if err := mgr.Remove(repo, "task-abcd1234-5678", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify worktree directory removed
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed")
	}

	// Verify branch still exists (done mode)
	out, err = exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) == 0 {
		t.Error("branch should still exist after Remove with deleteBranch=false")
	}

	// DB should show cleaned
	got, err = mgr.Get("task-abcd1234-5678")
	if err != nil {
		t.Fatalf("Get after Remove: %v", err)
	}
	if got == nil || got.CleanedAt == nil {
		t.Error("worktree should be marked as cleaned")
	}
}

func TestManager_RemoveWithBranchDelete(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-efgh5678-9012', 'proj-1', 'aborted', 'dev')`)

	mgr := &worktree.Manager{RootDir: wtRoot, DB: db, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-1", "task-efgh5678-9012", "", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Remove with branch delete (abort mode)
	if err := mgr.Remove(repo, "task-efgh5678-9012", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Verify branch is deleted
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) > 0 {
		t.Error("branch should be deleted after Remove with deleteBranch=true")
	}
}

func TestManager_RemoveIdempotent(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-noop0000-0000', 'proj-1', 'noop', 'dev')`)

	mgr := &worktree.Manager{RootDir: wtRoot, DB: db, GitBin: gitBin}

	// Remove for task with no worktree should be a no-op
	if err := mgr.Remove(repo, "task-noop0000-0000", false); err != nil {
		t.Fatalf("Remove no-op: %v", err)
	}
}

func TestManager_DefaultBranchPrefix(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-dflt1234-5678', 'proj-1', 'defaults', 'dev')`)

	mgr := &worktree.Manager{RootDir: wtRoot, DB: db, GitBin: gitBin}

	// Empty prefix and base branch should use defaults
	w, err := mgr.Create(repo, "proj-1", "task-dflt1234-5678", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if w.Branch != "boid/task-dfl" {
		t.Errorf("default branch: got %q, want %q", w.Branch, "boid/task-dfl")
	}
	if w.BaseBranch != "HEAD" {
		t.Errorf("default base branch: got %q, want %q", w.BaseBranch, "HEAD")
	}

	// Cleanup
	mgr.Remove(repo, "task-dflt1234-5678", true)
}
