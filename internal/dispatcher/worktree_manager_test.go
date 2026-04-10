package dispatcher_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/testutil"
)

const gitBin = "/usr/bin/git"

// initGitRepo creates a temporary git repo with an initial commit on branch "main".
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/main"}, // ensure branch is named "main"
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

// initGitRepoWithRemote creates a local repo with "origin" pointing to another local repo.
// Both have a "main" branch with an initial commit. Returns (localDir, remoteDir).
func initGitRepoWithRemote(t *testing.T) (string, string) {
	t.Helper()
	remoteDir := initGitRepo(t)
	localDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"remote", "add", "origin", remoteDir},
		{"fetch", "origin"},
		{"reset", "--hard", "origin/main"}, // create local "main" tracking origin/main
	} {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = localDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return localDir, remoteDir
}

func TestManager_CreateAndRemove(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	// Register project and task in DB
	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-abcd1234-5678', 'proj-1', 'test', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

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

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

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

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Remove for task with no worktree should be a no-op
	if err := mgr.Remove(repo, "task-noop0000-0000", false); err != nil {
		t.Fatalf("Remove no-op: %v", err)
	}
}

// TestResolveBase_* tests verify the resolveBaseBranch logic through Create().

func TestResolveBase_EmptyWithRemote(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rb000001-0001', 'proj-1', 'resolve base empty', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Empty baseBranch → should resolve to "origin/main" (remote exists)
	w, err := mgr.Create(local, "proj-1", "task-rb000001-0001", "boid/", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.BaseBranch != "origin/main" {
		t.Errorf("expected origin/main, got %q", w.BaseBranch)
	}
	mgr.Remove(local, "task-rb000001-0001", true)
}

func TestResolveBase_LocalBranchWithRemote(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rb000002-0001', 'proj-1', 'resolve base local', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// baseBranch = "main" → should resolve to "origin/main"
	w, err := mgr.Create(local, "proj-1", "task-rb000002-0001", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.BaseBranch != "origin/main" {
		t.Errorf("expected origin/main, got %q", w.BaseBranch)
	}
	mgr.Remove(local, "task-rb000002-0001", true)
}

func TestResolveBase_AlreadyOriginPrefixed(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rb000003-0001', 'proj-1', 'resolve base origin', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// baseBranch = "origin/main" → used as-is
	w, err := mgr.Create(local, "proj-1", "task-rb000003-0001", "boid/", "origin/main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.BaseBranch != "origin/main" {
		t.Errorf("expected origin/main, got %q", w.BaseBranch)
	}
	mgr.Remove(local, "task-rb000003-0001", true)
}

func TestResolveBase_NoRemoteFallback(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t) // no remote
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rb000004-0001', 'proj-1', 'resolve base no remote', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// No remote → fallback to local "main"
	w, err := mgr.Create(repo, "proj-1", "task-rb000004-0001", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.BaseBranch != "main" {
		t.Errorf("expected main, got %q", w.BaseBranch)
	}
	mgr.Remove(repo, "task-rb000004-0001", true)
}

func TestResolveBase_FetchFailureFallback(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	// Invalidate remote URL so fetch will fail
	exec.Command(gitBin, "-C", local, "remote", "set-url", "origin", "/nonexistent/invalid/path").Run()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rb000005-0001', 'proj-1', 'fetch failure', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// origin/main ref exists locally (from previous fetch), fetch will fail,
	// should fall back to local "main"
	w, err := mgr.Create(local, "proj-1", "task-rb000005-0001", "boid/", "")
	if err != nil {
		t.Fatalf("Create with fetch failure should succeed via fallback: %v", err)
	}
	if w.BaseBranch != "main" {
		t.Errorf("expected main after fallback, got %q", w.BaseBranch)
	}
	mgr.Remove(local, "task-rb000005-0001", true)
}

func TestManager_DefaultBranchPrefix(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-dflt1234-5678', 'proj-1', 'defaults', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Empty prefix and base branch should use defaults
	w, err := mgr.Create(repo, "proj-1", "task-dflt1234-5678", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if w.Branch != "boid/task-dfl" {
		t.Errorf("default branch: got %q, want %q", w.Branch, "boid/task-dfl")
	}
	if w.BaseBranch != "main" {
		t.Errorf("default base branch: got %q, want %q", w.BaseBranch, "main")
	}

	// Cleanup
	mgr.Remove(repo, "task-dflt1234-5678", true)
}
