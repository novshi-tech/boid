package dispatcher

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// fakeTaskLookupResolver is a multi-task stub for worktree_resolver tests.
// Defined here in package dispatcher (internal tests) to access allocateWorktree.
type fakeTaskLookupResolver struct {
	tasks map[string]*orchestrator.Task
}

func (f *fakeTaskLookupResolver) GetTask(id string) (*orchestrator.Task, error) {
	t, ok := f.tasks[id]
	if !ok {
		return nil, nil
	}
	return t, nil
}

func newTestDBForResolver(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d.Conn
}

func initGitRepoResolver(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
	} {
		cmd := exec.Command("/usr/bin/git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available outside worktree in this environment")
			}
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f := filepath.Join(dir, "README.md")
	os.WriteFile(f, []byte("# test"), 0o644)
	exec.Command("/usr/bin/git", "-C", dir, "add", ".").Run()
	if out, err := exec.Command("/usr/bin/git", "-C", dir, "commit", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

// TestAllocateWorktree_ChildTask_ForksFromParentHeadBranch verifies the
// resolver-level (P3) behaviour end-to-end: when allocateWorktree is called
// for a child task, it looks up the parent, computes ComputeHeadBranch, and
// passes it as ForkPoint so Create branches from the parent's worktree branch.
func TestAllocateWorktree_ChildTask_ForksFromParentHeadBranch(t *testing.T) {
	conn := newTestDBForResolver(t)
	repo := initGitRepoResolver(t)
	wtRoot := t.TempDir()

	// Simulate the parent's worktree branch "boid/parent00" with one extra commit.
	// parent task ID[:8] = "parent00" → ComputeHeadBranch(parent) = "boid/parent00"
	// (parent has ParentID != "" so it is itself a child).
	if out, err := exec.Command("/usr/bin/git", "-C", repo, "checkout", "-b", "boid/parent00").CombinedOutput(); err != nil {
		t.Fatalf("create boid/parent00: %v\n%s", err, out)
	}
	f := filepath.Join(repo, "parent_work.txt")
	os.WriteFile(f, []byte("parent work"), 0o644)
	exec.Command("/usr/bin/git", "-C", repo, "add", ".").Run()
	if out, err := exec.Command("/usr/bin/git", "-C", repo, "commit", "-m", "parent commit").CombinedOutput(); err != nil {
		t.Fatalf("parent commit: %v\n%s", err, out)
	}
	parentTip, err := exec.Command("/usr/bin/git", "-C", repo, "rev-parse", "boid/parent00").Output()
	if err != nil {
		t.Fatalf("rev-parse boid/parent00: %v", err)
	}
	exec.Command("/usr/bin/git", "-C", repo, "checkout", "main").Run()

	// Parent: has ParentID set → ComputeHeadBranch = "boid/parent00".
	parentTask := &orchestrator.Task{
		ID:         "parent0012345678",
		ProjectID:  "proj-resolver",
		ParentID:   "grandparent-root",
		BaseBranch: "main",
	}
	// Child: ParentID = parent's ID.
	childTask := &orchestrator.Task{
		ID:         "child00012345678",
		ProjectID:  "proj-resolver",
		ParentID:   "parent0012345678",
		BaseBranch: "main",
		Worktree:   true,
	}

	conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-resolver', ?)`, repo)
	conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior, parent_id) VALUES (?, 'proj-resolver', 'parent', 'executor', ?)`,
		"parent0012345678", "grandparent-root")
	conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior, parent_id) VALUES (?, 'proj-resolver', 'child', 'executor', ?)`,
		"child00012345678", "parent0012345678")

	mgr := &WorktreeManager{RootDir: wtRoot, DB: conn, GitBin: "/usr/bin/git"}
	r := &Runner{
		DB:        conn,
		Worktrees: mgr,
		TaskLookup: &fakeTaskLookupResolver{tasks: map[string]*orchestrator.Task{
			"parent0012345678": parentTask,
			"child00012345678": childTask,
		}},
	}

	spec := &orchestrator.JobSpec{
		TaskID:    "child00012345678",
		ProjectID: "proj-resolver",
		Visibility: orchestrator.Visibility{
			ProjectDir:  repo,
			UseWorktree: true,
		},
	}

	wtPath, err := r.allocateWorktree(spec)
	if err != nil {
		t.Fatalf("allocateWorktree: %v", err)
	}

	// Child worktree HEAD must match the parent branch tip.
	wtTip, err := exec.Command("/usr/bin/git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in child worktree: %v", err)
	}
	if strings.TrimSpace(string(wtTip)) != strings.TrimSpace(string(parentTip)) {
		t.Errorf("child worktree HEAD = %s, want %s (from boid/parent00)",
			strings.TrimSpace(string(wtTip)), strings.TrimSpace(string(parentTip)))
	}

	mgr.Remove(repo, "child00012345678", true)
}

// TestAllocateWorktree_RootTask_UseCheckoutBranch verifies that root tasks
// (ParentID == "") still get CheckoutBranch = task.BaseBranch (P2 retention).
// Uses a "feature" branch so "main" (already checked out in the repo's main
// worktree) does not conflict with the new worktree add.
func TestAllocateWorktree_RootTask_UseCheckoutBranch(t *testing.T) {
	conn := newTestDBForResolver(t)
	repo := initGitRepoResolver(t)
	wtRoot := t.TempDir()

	// Create a "feature" branch so the root task can check it out without
	// conflicting with the main worktree (which already holds "main").
	if out, err := exec.Command("/usr/bin/git", "-C", repo, "checkout", "-b", "feature").CombinedOutput(); err != nil {
		t.Fatalf("create feature branch: %v\n%s", err, out)
	}
	f2 := filepath.Join(repo, "feature.txt")
	os.WriteFile(f2, []byte("feature"), 0o644)
	exec.Command("/usr/bin/git", "-C", repo, "add", ".").Run()
	if out, err := exec.Command("/usr/bin/git", "-C", repo, "commit", "-m", "feature commit").CombinedOutput(); err != nil {
		t.Fatalf("feature commit: %v\n%s", err, out)
	}
	exec.Command("/usr/bin/git", "-C", repo, "checkout", "main").Run()

	rootTask := &orchestrator.Task{
		ID:         "rootrootabcd1234",
		ProjectID:  "proj-resolver2",
		BaseBranch: "feature",
		Worktree:   true,
		// ParentID == "" → root
	}

	conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('proj-resolver2', ?)`, repo)
	conn.Exec(`INSERT INTO tasks (id, project_id, title, behavior) VALUES (?, 'proj-resolver2', 'root', 'executor')`,
		"rootrootabcd1234")

	mgr := &WorktreeManager{RootDir: wtRoot, DB: conn, GitBin: "/usr/bin/git"}
	r := &Runner{
		DB:        conn,
		Worktrees: mgr,
		TaskLookup: &fakeTaskLookupResolver{tasks: map[string]*orchestrator.Task{
			"rootrootabcd1234": rootTask,
		}},
	}

	spec := &orchestrator.JobSpec{
		TaskID:    "rootrootabcd1234",
		ProjectID: "proj-resolver2",
		Visibility: orchestrator.Visibility{
			ProjectDir:  repo,
			UseWorktree: true,
		},
	}

	wtPath, err := r.allocateWorktree(spec)
	if err != nil {
		t.Fatalf("allocateWorktree for root task: %v", err)
	}

	// Root task: worktree HEAD should be on "feature" (CheckoutBranch path, P2).
	headOut, err := exec.Command("/usr/bin/git", "-C", wtPath, "symbolic-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(headOut)); got != "refs/heads/feature" {
		t.Errorf("root task worktree HEAD = %q, want refs/heads/feature", got)
	}

	mgr.Remove(repo, "rootrootabcd1234", false)
}
