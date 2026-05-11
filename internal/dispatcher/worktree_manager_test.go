package dispatcher_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
