package dispatcher_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
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
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available outside worktree in this environment")
			}
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

// TestRecreate_LocalAndRemoteBranchMissing verifies that Recreate succeeds even
// when the branch was never pushed and the local branch was deleted (rerun after
// abort). The branch is recreated from the recorded base branch — rerun has
// reset-and-retry semantics, so losing the previous commit is expected.
func TestRecreate_LocalAndRemoteBranchMissing(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-re2', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-re001234-0002', 'proj-re2', 'no remote branch', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(local, "proj-re2", "task-re001234-0002", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate abort → rerun: removes worktree and local branch; remote branch never existed.
	if err := mgr.CleanupForTask("task-re001234-0002", local, "aborted"); err != nil {
		t.Fatalf("CleanupForTask: %v", err)
	}

	recreated, err := mgr.Recreate(local, "task-re001234-0002")
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	if recreated == nil || recreated.Branch != w.Branch {
		t.Fatalf("recreated branch = %+v, want branch %q", recreated, w.Branch)
	}

	// Local branch should be restored, pointing at the base branch tip.
	headCmd := exec.Command(gitBin, "-C", local, "rev-parse", w.Branch)
	head, err := headCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v\n%s", w.Branch, err, head)
	}
	baseCmd := exec.Command(gitBin, "-C", local, "rev-parse", "origin/main")
	base, err := baseCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v\n%s", err, base)
	}
	if strings.TrimSpace(string(head)) != strings.TrimSpace(string(base)) {
		t.Errorf("recreated branch head %s, want base %s", strings.TrimSpace(string(head)), strings.TrimSpace(string(base)))
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

// TestRecreate_FetchesBaseBranch verifies that Recreate also fetches origin/<baseBranch>,
// so that the worktree sees commits that were added to the remote main after the worktree
// was cleaned. This is required for correct conflict resolution in reworking state.
func TestRecreate_FetchesBaseBranch(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, remote := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-rebb1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rebb0001-0001', 'proj-rebb1', 'fetch base branch', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Create worktree and push feature branch to remote.
	w, err := mgr.Create(local, "proj-rebb1", "task-rebb0001-0001", "boid/", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out, err := exec.Command(gitBin, "-C", local, "push", "origin", w.Branch).CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// Simulate done: remove worktree and delete local branch.
	if err := mgr.CleanupForTask("task-rebb0001-0001", local, "done"); err != nil {
		t.Fatalf("CleanupForTask: %v", err)
	}

	// Add a new commit to remote "main" (simulates upstream progress after worktree was cleaned).
	f := filepath.Join(remote, "new_file.txt")
	if err := os.WriteFile(f, []byte("new content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	exec.Command(gitBin, "-C", remote, "add", ".").Run()
	if out, err := exec.Command(gitBin, "-C", remote, "commit", "-m", "new commit on main").CombinedOutput(); err != nil {
		t.Fatalf("git commit on remote: %v\n%s", err, out)
	}

	// Get the new commit hash from remote main.
	out, err := exec.Command(gitBin, "-C", remote, "rev-parse", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, out)
	}
	newMainHash := strings.TrimSpace(string(out))

	// Recreate the worktree — should also fetch origin/main.
	recreated, err := mgr.Recreate(local, "task-rebb0001-0001")
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	// Verify that origin/main in the recreated worktree points to the new commit.
	out, err = exec.Command(gitBin, "-C", recreated.Path, "rev-parse", "origin/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse origin/main in worktree: %v\n%s", err, out)
	}
	gotHash := strings.TrimSpace(string(out))
	if gotHash != newMainHash {
		t.Errorf("origin/main should be updated after Recreate: got %s, want %s", gotHash, newMainHash)
	}
}

// TestRecreate_LocalBranchFallback verifies that Recreate succeeds using the local branch
// when the remote fetch fails (e.g. branch was never pushed, or remote is unreachable).
// The worktree dir is removed (Remove with deleteBranch=false) to simulate a cleaned
// record while keeping the local branch alive.
func TestRecreate_LocalBranchFallback(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t) // no remote: any fetch will fail
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-rlf1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rlf10001-0001', 'proj-rlf1', 'local fallback', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// Create worktree (local branch is created).
	w, err := mgr.Create(repo, "proj-rlf1", "task-rlf10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Remove worktree dir without deleting local branch; marks cleaned_at in DB.
	if err := mgr.Remove(repo, "task-rlf10001-0001", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatal("worktree dir should be removed after Remove")
	}

	// Recreate should succeed via local branch fallback (fetch fails: no remote).
	recreated, err := mgr.Recreate(repo, "task-rlf10001-0001")
	if err != nil {
		t.Fatalf("Recreate should succeed via local branch fallback: %v", err)
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

	// DB: cleaned_at should be NULL.
	got, err := mgr.Get("task-rlf10001-0001")
	if err != nil {
		t.Fatalf("Get after Recreate: %v", err)
	}
	if got == nil || got.CleanedAt != nil {
		t.Error("cleaned_at should be NULL after Recreate")
	}
}

// TestRecreate_BaseBranchUnresolvable verifies that when the task branch is gone
// AND the recorded base branch is also unresolvable, Recreate surfaces an explicit
// error. This is a pathological case — "main" normally exists — but it guards
// the fallback path introduced for rerun-after-abort.
func TestRecreate_BaseBranchUnresolvable(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t) // no remote: any fetch will fail
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-rbm1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rbm10001-0001', 'proj-rbm1', 'both missing', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	_, err := mgr.Create(repo, "proj-rbm1", "task-rbm10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Remove(repo, "task-rbm10001-0001", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Force an unresolvable base branch so the rerun fallback has nothing to branch from.
	if _, err := db.Conn.Exec(`UPDATE worktrees SET base_branch = 'nonexistent-base' WHERE task_id = 'task-rbm10001-0001'`); err != nil {
		t.Fatalf("update base_branch: %v", err)
	}

	_, err = mgr.Recreate(repo, "task-rbm10001-0001")
	if err == nil {
		t.Fatal("Recreate should return an error when base branch cannot be resolved")
	}
	if !strings.Contains(err.Error(), "base branch") {
		t.Errorf("error should mention base branch resolution failure, got: %v", err)
	}
}

// TestManager_Create_EnsuresBoidDir verifies that Create makes a `.boid/` directory
// in the new worktree even when the source repo doesn't track one. The sandbox
// bind-mounts <project>/.boid → <worktree>/.boid; if the target dir is missing
// and the worktree is mounted readonly (plan tasks), the bind fails with EROFS.
func TestManager_Create_EnsuresBoidDir(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t) // initial commit has only README.md, no .boid/
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-bdir0001-0001', 'proj-1', 'ensure boid dir', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-1", "task-bdir0001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	boidDir := filepath.Join(w.Path, ".boid")
	info, err := os.Stat(boidDir)
	if err != nil {
		t.Fatalf(".boid dir should exist in worktree: %v", err)
	}
	if !info.IsDir() {
		t.Errorf(".boid should be a directory")
	}

	mgr.Remove(repo, "task-bdir0001-0001", true)
}

// TestManager_Recreate_EnsuresBoidDir verifies that Recreate also makes `.boid/`
// in the rebuilt worktree (same rationale as Create — recreate is used on rerun
// and reopen, both of which feed back into the sandbox).
func TestManager_Recreate_EnsuresBoidDir(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-rbdir001-0001', 'proj-1', 'recreate ensure boid', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	if _, err := mgr.Create(repo, "proj-1", "task-rbdir001-0001", "boid/", "HEAD"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Remove(repo, "task-rbdir001-0001", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	recreated, err := mgr.Recreate(repo, "task-rbdir001-0001")
	if err != nil {
		t.Fatalf("Recreate: %v", err)
	}

	boidDir := filepath.Join(recreated.Path, ".boid")
	info, err := os.Stat(boidDir)
	if err != nil {
		t.Fatalf(".boid dir should exist in recreated worktree: %v", err)
	}
	if !info.IsDir() {
		t.Errorf(".boid should be a directory")
	}
}

// TestManager_EnsureBindingTargets_CreatesWorktreeSubdirs verifies that
// EnsureBindingTargets pre-mkdirs additional_bindings targets that live under
// the worktree, so a later readonly bind-remount of the worktree does not cause
// EROFS when the sandbox setup script tries to mkdir the target.
//
// Repro: readonly:true + worktree:true + a binding whose target is ${WORKTREE}/sub
// would fail because render.go bind-mounts and ro-remounts the worktree before
// `additionalBindingMounts` runs, leaving subsequent `mkdir -p $ROOT<worktree>/sub`
// to hit the parent's readonly bind. Pre-mkdir on the host (writable) sidesteps
// the trap entirely — `mount --bind` only needs the target to exist.
func TestManager_EnsureBindingTargets_CreatesWorktreeSubdirs(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-ebt10001-0001', 'proj-1', 'ensure binding targets', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	w, err := mgr.Create(repo, "proj-1", "task-ebt10001-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Remove(repo, "task-ebt10001-0001", true) })

	bindings := []orchestrator.BindMount{
		// 1) directory target under the worktree → mkdir-p the target dir
		{Source: "/opt/external-build-cache", Target: "${WORKTREE}/build"},
		// 2) file target under the worktree → mkdir-p the parent dir
		{Source: "${PROJECT_WORKDIR}/etc/global.json", Target: "${WORKTREE}/etc/global.json", IsFile: true},
		// 3) deeply nested directory target under the worktree → mkdir-p
		{Source: "/opt/another", Target: "${WORKTREE}/deeply/nested/dir"},
		// 4) target outside the worktree → must be skipped (not created)
		{Source: "/opt/sock", Target: "/var/run/some.sock", IsFile: true},
		// 5) target omitted (defaults to source, outside worktree) → skipped
		{Source: "/opt/external-tool"},
	}

	if err := mgr.EnsureBindingTargets(w.Path, bindings, repo); err != nil {
		t.Fatalf("EnsureBindingTargets: %v", err)
	}

	// Case 1: directory target exists as a directory.
	if info, err := os.Stat(filepath.Join(w.Path, "build")); err != nil {
		t.Errorf("expected <worktree>/build to exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("<worktree>/build should be a directory")
	}

	// Case 2: file target's parent dir exists. The target file itself does not
	// have to be created — `mount --bind` does not need a pre-existing file when
	// the source is a regular file, and the sandbox setup script touches the
	// target when needed. What we MUST avoid is EROFS on `mkdir -p <parent>`.
	if info, err := os.Stat(filepath.Join(w.Path, "etc")); err != nil {
		t.Errorf("expected <worktree>/etc to exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("<worktree>/etc should be a directory")
	}

	// Case 3: deeply nested directory exists.
	if info, err := os.Stat(filepath.Join(w.Path, "deeply", "nested", "dir")); err != nil {
		t.Errorf("expected nested dir to exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("nested target should be a directory")
	}

	// Case 4 + 5: targets outside the worktree must not have been auto-created
	// under the worktree.
	if _, err := os.Stat(filepath.Join(w.Path, "var")); !os.IsNotExist(err) {
		t.Errorf("worktree should not contain stray %q dir, got err=%v", "var", err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "opt")); !os.IsNotExist(err) {
		t.Errorf("worktree should not contain stray %q dir, got err=%v", "opt", err)
	}
}

// TestManager_EnsureBindingTargets_NonGit verifies the pre-mkdir semantics on
// a plain directory, without depending on git availability. Mirrors the cases
// in TestManager_EnsureBindingTargets_CreatesWorktreeSubdirs so the
// EROFS-prevention contract is enforced even in environments where the git
// fixture is unavailable (e.g. sandboxed CI shells).
func TestManager_EnsureBindingTargets_NonGit(t *testing.T) {
	db := testutil.NewTestDB(t)
	wt := t.TempDir() // stand-in for a freshly created worktree
	projectDir := t.TempDir()

	mgr := &dispatcher.WorktreeManager{RootDir: t.TempDir(), DB: db.Conn, GitBin: gitBin}

	bindings := []orchestrator.BindMount{
		// directory target under the worktree
		{Source: "/opt/external-build-cache", Target: "${WORKTREE}/build"},
		// file target under the worktree
		{Source: "${PROJECT_WORKDIR}/etc/global.json", Target: "${WORKTREE}/etc/global.json", IsFile: true},
		// nested directory target
		{Source: "/opt/another", Target: "${WORKTREE}/deeply/nested/dir"},
		// target outside the worktree → must be skipped
		{Source: "/opt/sock", Target: "/var/run/some.sock", IsFile: true},
		// target omitted → defaults to source, outside worktree → skipped
		{Source: "/opt/external-tool"},
	}

	if err := mgr.EnsureBindingTargets(wt, bindings, projectDir); err != nil {
		t.Fatalf("EnsureBindingTargets: %v", err)
	}

	if info, err := os.Stat(filepath.Join(wt, "build")); err != nil {
		t.Errorf("<worktree>/build should exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("<worktree>/build should be a directory")
	}
	if info, err := os.Stat(filepath.Join(wt, "etc")); err != nil {
		t.Errorf("<worktree>/etc should exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("<worktree>/etc should be a directory")
	}
	if info, err := os.Stat(filepath.Join(wt, "deeply", "nested", "dir")); err != nil {
		t.Errorf("nested dir should exist: %v", err)
	} else if !info.IsDir() {
		t.Errorf("nested target should be a directory")
	}
	// Targets that do not live under the worktree must not pollute the worktree.
	if entries, err := os.ReadDir(wt); err != nil {
		t.Errorf("read worktree: %v", err)
	} else {
		for _, e := range entries {
			switch e.Name() {
			case "build", "etc", "deeply":
				// expected
			default:
				t.Errorf("unexpected entry created under worktree: %q", e.Name())
			}
		}
	}
}

// TestManager_EnsureBindingTargets_RejectsEscapeNonGit verifies that
// path-traversal targets do not get created on the host, without needing git.
func TestManager_EnsureBindingTargets_RejectsEscapeNonGit(t *testing.T) {
	db := testutil.NewTestDB(t)
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	mgr := &dispatcher.WorktreeManager{RootDir: t.TempDir(), DB: db.Conn, GitBin: gitBin}

	bindings := []orchestrator.BindMount{
		{Source: "/opt/x", Target: "${WORKTREE}/../sibling"},
	}
	if err := mgr.EnsureBindingTargets(wt, bindings, ""); err != nil {
		t.Fatalf("EnsureBindingTargets: %v", err)
	}
	sibling := filepath.Join(parent, "sibling")
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("escape target must NOT be created on host, but found: %v", err)
	}
}

// TestManager_EnsureBindingTargets_EmptyAndNil verifies that EnsureBindingTargets
// is a no-op when there are no bindings or the worktree path is empty.
func TestManager_EnsureBindingTargets_EmptyAndNil(t *testing.T) {
	db := testutil.NewTestDB(t)
	mgr := &dispatcher.WorktreeManager{RootDir: t.TempDir(), DB: db.Conn, GitBin: gitBin}

	if err := mgr.EnsureBindingTargets("", nil, ""); err != nil {
		t.Errorf("empty inputs: unexpected error: %v", err)
	}
	if err := mgr.EnsureBindingTargets(t.TempDir(), nil, ""); err != nil {
		t.Errorf("nil bindings: unexpected error: %v", err)
	}
	if err := mgr.EnsureBindingTargets("", []orchestrator.BindMount{{Source: "/x", Target: "/y"}}, ""); err != nil {
		t.Errorf("empty worktree: unexpected error: %v", err)
	}
}

// TestManager_EnsureBindingTargets_RejectsEscape verifies that EnsureBindingTargets
// does not mkdir for targets that *appear* to be under the worktree via path
// traversal (`${WORKTREE}/../sibling`). Such bindings simply don't fall under the
// worktree and must be ignored by the pre-mkdir step. The rest of the binding
// stack (renderMount) handles the actual mount; pre-mkdir is purely about
// avoiding EROFS for genuine worktree-internal targets.
func TestManager_EnsureBindingTargets_RejectsEscape(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-ebtesc01-0001', 'proj-1', 'ensure binding targets escape', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}
	w, err := mgr.Create(repo, "proj-1", "task-ebtesc01-0001", "boid/", "HEAD")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Remove(repo, "task-ebtesc01-0001", true) })

	// Sibling path that escapes the worktree via ${WORKTREE}/../sibling.
	bindings := []orchestrator.BindMount{
		{Source: "/opt/x", Target: "${WORKTREE}/../sibling"},
	}
	if err := mgr.EnsureBindingTargets(w.Path, bindings, repo); err != nil {
		t.Fatalf("EnsureBindingTargets: %v", err)
	}

	siblingDir := filepath.Join(filepath.Dir(w.Path), "sibling")
	if _, err := os.Stat(siblingDir); !os.IsNotExist(err) {
		t.Errorf("escape target must NOT be created on host, but found: %v", err)
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

// TestCreate_NonexistentBaseBranch verifies that Create returns an early error when
// base_branch does not exist either locally or on origin, preventing silent DWIM failures.
func TestCreate_NonexistentBaseBranch(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, _ := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-neb1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-neb10001-0001', 'proj-neb1', 'nonexistent base', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	_, err := mgr.Create(local, "proj-neb1", "task-neb10001-0001", "boid/", "nonexistent-branch")
	if err == nil {
		t.Fatal("Create should return an error for a nonexistent base_branch")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention branch not found, got: %v", err)
	}
}

// TestCreate_StaleFetchFailure verifies that Create returns an error when origin/<base>
// exists as a stale local remote-tracking ref but the branch has been deleted on remote
// and no local branch by that name exists. This is the silent-DWIM failure scenario.
func TestCreate_StaleFetchFailure(t *testing.T) {
	db := testutil.NewTestDB(t)
	local, remote := initGitRepoWithRemote(t)
	wtRoot := t.TempDir()

	// Create the branch on remote and fetch it into local (creates stale origin/stale-branch ref).
	exec.Command(gitBin, "-C", remote, "checkout", "-b", "stale-branch").Run()
	f := filepath.Join(remote, "stale.txt")
	os.WriteFile(f, []byte("stale"), 0o644)
	exec.Command(gitBin, "-C", remote, "add", ".").Run()
	exec.Command(gitBin, "-C", remote, "commit", "-m", "stale commit").Run()
	if out, err := exec.Command(gitBin, "-C", local, "fetch", "origin", "stale-branch").CombinedOutput(); err != nil {
		t.Fatalf("fetch stale-branch: %v\n%s", err, out)
	}
	// Delete the branch from remote (stale ref remains locally).
	exec.Command(gitBin, "-C", remote, "checkout", "main").Run()
	exec.Command(gitBin, "-C", remote, "branch", "-D", "stale-branch").Run()
	// Invalidate remote URL so any fetch will fail.
	exec.Command(gitBin, "-C", local, "remote", "set-url", "origin", "/nonexistent/invalid/path").Run()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-sff1', ?)`, local)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-sff10001-0001', 'proj-sff1', 'stale fetch failure', 'dev')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// origin/stale-branch exists locally (stale), but local branch "stale-branch" does not exist.
	// Fetch will fail. Create must return an error rather than silently using the stale ref.
	_, err := mgr.Create(local, "proj-sff1", "task-sff10001-0001", "boid/", "stale-branch")
	if err == nil {
		t.Fatal("Create should return an error when fetch fails and no local branch exists")
	}
}

// ---- Phase 2-2: case 3 base branch auto-create + case 1 HEAD guard ----

// TestCreate_Case3_CreatesBaseBranchFromHead verifies that Create succeeds
// when the requested base branch is unknown to both local and origin. The
// expected behaviour is "create it from HEAD before allocating the worktree",
// matching the dispatcher-side mitigation for ClassifyBaseBranch's Case3NotFound.
func TestCreate_Case3_CreatesBaseBranchFromHead(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t) // HEAD on main, no other branches
	wtRoot := t.TempDir()

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-p22-1', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-p2200001-0001', 'proj-p22-1', 'case3', 'supervisor')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	// "release-2026" exists neither locally nor on origin. Create must
	// auto-create it from HEAD and allocate a worktree based on it.
	w, err := mgr.Create(repo, "proj-p22-1", "task-p2200001-0001", "boid/", "release-2026")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.BaseBranch != "release-2026" {
		t.Errorf("BaseBranch = %q, want %q", w.BaseBranch, "release-2026")
	}

	// The newly created base branch must exist locally on the project repo.
	out, err := exec.Command(gitBin, "-C", repo, "branch", "--list", "release-2026").CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Errorf("release-2026 should be created on the project repo as a local branch")
	}
}

// TestCreate_Case3_DetachedHead_Errors verifies the detached-HEAD guard:
// creating a base branch from a detached commit is refused because the
// resulting branch would not correspond to mainline history.
func TestCreate_Case3_DetachedHead_Errors(t *testing.T) {
	db := testutil.NewTestDB(t)
	repo := initGitRepo(t)
	wtRoot := t.TempDir()

	// Move HEAD to a detached state on the current tip.
	out, err := exec.Command(gitBin, "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	hash := strings.TrimSpace(string(out))
	if out, err := exec.Command(gitBin, "-C", repo, "checkout", "--detach", hash).CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %v\n%s", err, out)
	}

	db.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-p22-2', ?)`, repo)
	db.Conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES ('task-p2200002-0001', 'proj-p22-2', 'case3 detached', 'supervisor')`)

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: db.Conn, GitBin: gitBin}

	_, err = mgr.Create(repo, "proj-p22-2", "task-p2200002-0001", "boid/", "release-2026")
	if err == nil {
		t.Fatal("expected Create to fail with detached HEAD, got nil")
	}
	if !strings.Contains(err.Error(), "detached HEAD") {
		t.Errorf("error message %q should mention detached HEAD", err.Error())
	}
}

// TestEnforceHeadOnBaseBranch_Match accepts the dispatch when HEAD matches.
func TestEnforceHeadOnBaseBranch_Match(t *testing.T) {
	repo := initGitRepo(t)
	mgr := &dispatcher.WorktreeManager{GitBin: gitBin}
	if err := mgr.EnforceHeadOnBaseBranch(repo, "main"); err != nil {
		t.Errorf("expected nil error for HEAD on main, got %v", err)
	}
	if err := mgr.EnforceHeadOnBaseBranch(repo, "origin/main"); err != nil {
		t.Errorf("expected origin/main to be treated as main, got %v", err)
	}
}

// TestEnforceHeadOnBaseBranch_Mismatch rejects dispatch when HEAD has been
// moved to a different branch since task creation.
func TestEnforceHeadOnBaseBranch_Mismatch(t *testing.T) {
	repo := initGitRepo(t)
	// Create a feature branch and check it out.
	if out, err := exec.Command(gitBin, "-C", repo, "checkout", "-b", "feature").CombinedOutput(); err != nil {
		t.Fatalf("checkout -b feature: %v\n%s", err, out)
	}

	mgr := &dispatcher.WorktreeManager{GitBin: gitBin}
	err := mgr.EnforceHeadOnBaseBranch(repo, "main")
	if err == nil {
		t.Fatal("expected error for HEAD on feature but task base_branch=main")
	}
	if !strings.Contains(err.Error(), "HEAD guard") {
		t.Errorf("error should mention HEAD guard, got %v", err)
	}
}

// TestEnforceHeadOnBaseBranch_DetachedRejected rejects detached HEAD as a
// case 1 mismatch (case 1 by definition implies a real branch).
func TestEnforceHeadOnBaseBranch_DetachedRejected(t *testing.T) {
	repo := initGitRepo(t)
	out, err := exec.Command(gitBin, "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	hash := strings.TrimSpace(string(out))
	if out, err := exec.Command(gitBin, "-C", repo, "checkout", "--detach", hash).CombinedOutput(); err != nil {
		t.Fatalf("checkout --detach: %v\n%s", err, out)
	}

	mgr := &dispatcher.WorktreeManager{GitBin: gitBin}
	err = mgr.EnforceHeadOnBaseBranch(repo, "main")
	if err == nil {
		t.Fatal("expected error for detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("error should mention detached, got %v", err)
	}
}

// TestEnforceHeadOnBaseBranch_EmptyBaseBranch is a no-op for tasks without a
// recorded base_branch (existing legacy tasks).
func TestEnforceHeadOnBaseBranch_EmptyBaseBranch(t *testing.T) {
	repo := initGitRepo(t)
	mgr := &dispatcher.WorktreeManager{GitBin: gitBin}
	if err := mgr.EnforceHeadOnBaseBranch(repo, ""); err != nil {
		t.Errorf("expected nil for empty base_branch, got %v", err)
	}
}

// ---- end Phase 2-2 ----
