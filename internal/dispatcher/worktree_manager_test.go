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

	// Remove 低レベル API: deleteBranch=false の場合
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

func TestManager_CleanupForTask_DoneDeletesBranch(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-cfd1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-cfd10001-0001', 'proj-cfd1', 'done task', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-cfd1", "task-cfd10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.CleanupForTask("task-cfd10001-0001", repo, "done"); err != nil {
		t.Fatalf("CleanupForTask done: %v", err)
	}

	// worktree ディレクトリが削除されていること
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed after done cleanup")
	}

	// ブランチが削除されていること
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("branch should be deleted after done cleanup, got: %q", string(out))
	}
}

func TestManager_CleanupForTask_AbortedDeletesBranch(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-cfa1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-cfa10001-0001', 'proj-cfa1', 'aborted task', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-cfa1", "task-cfa10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.CleanupForTask("task-cfa10001-0001", repo, "aborted"); err != nil {
		t.Fatalf("CleanupForTask aborted: %v", err)
	}

	// worktree ディレクトリが削除されていること
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed after aborted cleanup")
	}

	// ブランチが削除されていること
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("branch should be deleted after aborted cleanup, got: %q", string(out))
	}
}

func TestManager_CleanupForTask_PendingNoop(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-cfp1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-cfp10001-0001', 'proj-cfp1', 'executing task', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-cfp1", "task-cfp10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.CleanupForTask("task-cfp10001-0001", repo, "executing"); err != nil {
		t.Fatalf("CleanupForTask executing: %v", err)
	}

	// worktree ディレクトリが残っていること
	if _, err := os.Stat(w.Path); err != nil {
		t.Errorf("worktree dir should still exist for non-terminal status: %v", err)
	}

	// ブランチが残っていること
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) == 0 {
		t.Error("branch should still exist for non-terminal status")
	}

	// クリーンアップ
	mgr.Remove(repo, "task-cfp10001-0001", true)
}

// TestRecreate_Success verifies that after done (worktree and local branch deleted),
// Recreate reconstructs the worktree from the remote branch and restores the local branch.
func TestRecreate_Success(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-re1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-re001234-0001', 'proj-re1', 'recreate test', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Create worktree and push branch to remote.
	w, err := mgr.Create(local, "proj-re1", "task-re001234-0001", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out, err := exec.Command(gitBin, "-C", local, "push", "origin", w.Branch).CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// Simulate done: remove worktree and delete local branch.
	if err := mgr.CleanupForTask("task-re001234-0001", local, "done"); err != nil {
		t.Fatalf("CleanupForTask: %v", err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatal("worktree dir should be removed after done")
	}

	// Recreate from remote branch.
	recreated, err := mgr.Recreate(local, "task-re001234-0001")
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	// Worktree directory should exist.
	if _, err := os.Stat(recreated.Path); err != nil {
		t.Errorf("recreated worktree dir should exist: %v", err)
	}

	// .git should be a file (worktree metadata), not a directory.
	gitFile := filepath.Join(recreated.Path, ".git")
	info, err := os.Stat(gitFile)
	if err != nil {
		t.Fatalf(".git file should exist after recreate: %v", err)
	}
	if info.IsDir() {
		t.Error(".git should be a file in worktree, not a directory")
	}

	// Local branch should be restored.
	out, err := exec.Command(gitBin, "-C", local, "branch", "--list", recreated.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) == 0 {
		t.Error("local branch should be restored after Recreate")
	}

	// DB: cleaned_at should be NULL.
	got, err := mgr.Get("task-re001234-0001")
	if err != nil {
		t.Fatalf("Get after Recreate: %v", err)
	}
	if got == nil || got.CleanedAt != nil {
		t.Error("cleaned_at should be NULL after Recreate")
	}
}

// TestRecreate_RemoteBranchMissing verifies that Recreate returns an error when
// the remote branch does not exist (branch was never pushed).
func TestRecreate_RemoteBranchMissing(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-re2', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-re001234-0002', 'proj-re2', 'no remote branch', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Create worktree but do NOT push to remote.
	_, err := mgr.Create(local, "proj-re2", "task-re001234-0002", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate done: removes worktree and local branch; remote branch never existed.
	if err := mgr.CleanupForTask("task-re001234-0002", local, "done"); err != nil {
		t.Fatalf("CleanupForTask: %v", err)
	}

	// Recreate should fail because fetch from remote will fail.
	_, err = mgr.Recreate(local, "task-re001234-0002")
	if err == nil {
		t.Error("Recreate should return an error when remote branch does not exist")
	}
}

// TestRecreate_DBConsistency verifies that cleaned_at is NULL in the DB after a successful Recreate.
func TestRecreate_DBConsistency(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-re3', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-re001234-0003', 'proj-re3', 'db consistency', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(local, "proj-re3", "task-re001234-0003", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out, err := exec.Command(gitBin, "-C", local, "push", "origin", w.Branch).CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	if err := mgr.CleanupForTask("task-re001234-0003", local, "done"); err != nil {
		t.Fatalf("CleanupForTask: %v", err)
	}

	// Verify cleaned_at is set before Recreate.
	before, err := mgr.Get("task-re001234-0003")
	if err != nil {
		t.Fatalf("Get before Recreate: %v", err)
	}
	if before == nil || before.CleanedAt == nil {
		t.Fatal("cleaned_at should be set after done cleanup")
	}

	if _, err := mgr.Recreate(local, "task-re001234-0003"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	// Verify cleaned_at is NULL after Recreate.
	after, err := mgr.Get("task-re001234-0003")
	if err != nil {
		t.Fatalf("Get after Recreate: %v", err)
	}
	if after == nil {
		t.Fatal("worktree record should exist after Recreate")
	}
	if after.CleanedAt != nil {
		t.Errorf("cleaned_at should be NULL after Recreate, got %v", after.CleanedAt)
	}
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
