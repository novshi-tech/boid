package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

const gcTestGitBin = "/usr/bin/git"

// initGitRepoForGC creates a temporary git repository with an initial commit.
func initGitRepoForGC(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command(gcTestGitBin, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f := filepath.Join(dir, "README.md")
	os.WriteFile(f, []byte("# test"), 0o644)
	exec.Command(gcTestGitBin, "-C", dir, "add", ".").Run()
	cmd := exec.Command(gcTestGitBin, "-C", dir, "commit", "-m", "initial")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

func TestGCTasks_DeletesDoneAndAborted(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create done task: %v", err)
	}

	abortedTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Aborted Task", Behavior: "dev", Status: orchestrator.TaskStatusAborted}
	if err := orchestrator.CreateTask(d.Conn, abortedTask); err != nil {
		t.Fatalf("create aborted task: %v", err)
	}

	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending Task", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create pending task: %v", err)
	}

	// アクションを作成（done タスクに紐付く）
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: doneTask.ID, Type: "start"}); err != nil {
		t.Fatalf("create action: %v", err)
	}
	if err := orchestrator.CreateAction(d.Conn, &orchestrator.Action{TaskID: doneTask.ID, Type: "done"}); err != nil {
		t.Fatalf("create action: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)
	result, err := gcStore.GC(0, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 2 {
		t.Fatalf("expected 2 deleted tasks, got %d", result.Tasks)
	}
	if result.Actions != 2 {
		t.Fatalf("expected 2 deleted actions, got %d", result.Actions)
	}

	// pending タスクは残っている
	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != pendingTask.ID {
		t.Fatalf("expected pending task to remain, got %s", tasks[0].ID)
	}

	// done タスクのアクションが削除されている
	actions, err := orchestrator.ListActionsByTask(d.Conn, doneTask.ID)
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions after GC, got %d", len(actions))
	}
}

func TestGCTasks_EmptyStatuses(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// GCTasks を直接呼び出して空 statuses を確認
	result, err := orchestrator.GCTasks(d.Conn, []string{}, 0, false, nil)
	if err != nil {
		t.Fatalf("gc tasks: %v", err)
	}
	if result.Tasks != 0 {
		t.Fatalf("expected 0 deleted tasks for empty statuses, got %d", result.Tasks)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected task to remain, got %d", len(tasks))
	}
}

func TestGCTasks_OlderThanFilter(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	oldTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Old Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, oldTask); err != nil {
		t.Fatalf("create old task: %v", err)
	}
	recentTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Recent Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, recentTask); err != nil {
		t.Fatalf("create recent task: %v", err)
	}

	// old タスクの updated_at を 60 日前に設定
	sixtyDaysAgo := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := d.Conn.Exec(
		`UPDATE tasks SET updated_at = ? WHERE id = ?`,
		sixtyDaysAgo, oldTask.ID,
	); err != nil {
		t.Fatalf("update updated_at: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)

	// dry-run: 30日以上経過したものが1件あるはず
	result, err := gcStore.GC(30*24*time.Hour, true, nil)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("dry-run: expected 1, got %d", result.Tasks)
	}

	// タスクはまだ存在する（dry-run なので削除されていない）
	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("after dry-run: expected 2 tasks, got %d", len(tasks))
	}

	// 実際に削除
	result, err = gcStore.GC(30*24*time.Hour, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task, got %d", result.Tasks)
	}

	// recent タスクは残っている
	tasks, err = orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != recentTask.ID {
		t.Fatalf("expected recent task to remain")
	}
}

func TestGCTasks_NothingToDelete(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn)
	result, err := gcStore.GC(0, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 0 {
		t.Fatalf("expected 0 deleted tasks, got %d", result.Tasks)
	}
}

func TestGCTasks_WorktreeDiskCleanup(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := initGitRepoForGC(t)
	wtRoot := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: repo}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Done Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
	}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: d.Conn, GitBin: gcTestGitBin}
	w, err := mgr.Create(repo, "proj-1", doneTask.ID, "boid/", "HEAD")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	// worktree ディレクトリが存在することを確認
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("worktree dir should exist before GC: %v", err)
	}

	resolveProjectDir := func(projectID string) (string, error) {
		proj, err := orchestrator.GetProject(d.Conn, projectID)
		if err != nil {
			return "", err
		}
		return proj.WorkDir, nil
	}
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin)

	result, err := gcStore.GC(0, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task, got %d", result.Tasks)
	}

	// worktree ディレクトリが削除されていることを確認
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed after GC, err: %v", err)
	}
}

func TestGCTasks_WorktreeDiskCleanup_DoneDeletesBranch(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := initGitRepoForGC(t)
	wtRoot := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-gcd1", WorkDir: repo}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{
		ProjectID: "proj-gcd1",
		Title:     "Done Task Branch Delete",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
	}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: d.Conn, GitBin: gcTestGitBin}
	w, err := mgr.Create(repo, "proj-gcd1", doneTask.ID, "boid/", "HEAD")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	resolveProjectDir := func(projectID string) (string, error) {
		proj, err := orchestrator.GetProject(d.Conn, projectID)
		if err != nil {
			return "", err
		}
		return proj.WorkDir, nil
	}
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin)

	result, err := gcStore.GC(0, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task, got %d", result.Tasks)
	}

	// worktree ディレクトリが削除されていることを確認
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed after GC, err: %v", err)
	}

	// ブランチが削除されていることを確認
	out, err := exec.Command(gcTestGitBin, "-C", repo, "branch", "--list", w.Branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("branch should be deleted after GC of done task, got: %q", string(out))
	}
}

func TestGCTasks_WorktreeDiskCleanup_DryRun(t *testing.T) {
	d := testutil.NewTestDB(t)
	repo := initGitRepoForGC(t)
	wtRoot := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: repo}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Done Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusDone,
	}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	mgr := &dispatcher.WorktreeManager{RootDir: wtRoot, DB: d.Conn, GitBin: gcTestGitBin}
	w, err := mgr.Create(repo, "proj-1", doneTask.ID, "boid/", "HEAD")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	resolveProjectDir := func(projectID string) (string, error) {
		proj, err := orchestrator.GetProject(d.Conn, projectID)
		if err != nil {
			return "", err
		}
		return proj.WorkDir, nil
	}
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin)

	// dry-run: ディスク操作はスキップされる
	result, err := gcStore.GC(0, true, nil)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("dry-run: expected 1 task, got %d", result.Tasks)
	}

	// worktree ディレクトリが残っていることを確認（dry-run なので削除されない）
	if _, err := os.Stat(w.Path); err != nil {
		t.Errorf("worktree dir should still exist after dry-run GC: %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestGCTasks_EphemeralOnly(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	ephemeralTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Ephemeral Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: true}
	if err := orchestrator.CreateTask(d.Conn, ephemeralTask); err != nil {
		t.Fatalf("create ephemeral task: %v", err)
	}

	normalTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Normal Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: false}
	if err := orchestrator.CreateTask(d.Conn, normalTask); err != nil {
		t.Fatalf("create normal task: %v", err)
	}

	result, err := orchestrator.GCTasks(d.Conn, []string{"done", "aborted"}, 0, false, boolPtr(true))
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task (ephemeral only), got %d", result.Tasks)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != normalTask.ID {
		t.Fatalf("expected non-ephemeral task to remain, got %s", tasks[0].ID)
	}
}

func TestGCTasks_NonEphemeralOnly(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	ephemeralTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Ephemeral Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: true}
	if err := orchestrator.CreateTask(d.Conn, ephemeralTask); err != nil {
		t.Fatalf("create ephemeral task: %v", err)
	}

	normalTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Normal Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: false}
	if err := orchestrator.CreateTask(d.Conn, normalTask); err != nil {
		t.Fatalf("create normal task: %v", err)
	}

	result, err := orchestrator.GCTasks(d.Conn, []string{"done", "aborted"}, 0, false, boolPtr(false))
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 1 {
		t.Fatalf("expected 1 deleted task (non-ephemeral only), got %d", result.Tasks)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != ephemeralTask.ID {
		t.Fatalf("expected ephemeral task to remain, got %s", tasks[0].ID)
	}
}

func TestGCTasks_NoFilter(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	ephemeralTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Ephemeral Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: true}
	if err := orchestrator.CreateTask(d.Conn, ephemeralTask); err != nil {
		t.Fatalf("create ephemeral task: %v", err)
	}

	normalTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Normal Done", Behavior: "dev", Status: orchestrator.TaskStatusDone, Ephemeral: false}
	if err := orchestrator.CreateTask(d.Conn, normalTask); err != nil {
		t.Fatalf("create normal task: %v", err)
	}

	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create pending task: %v", err)
	}

	result, err := orchestrator.GCTasks(d.Conn, []string{"done", "aborted"}, 0, false, nil)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 2 {
		t.Fatalf("expected 2 deleted tasks (no filter), got %d", result.Tasks)
	}

	tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 remaining task, got %d", len(tasks))
	}
	if tasks[0].ID != pendingTask.ID {
		t.Fatalf("expected pending task to remain, got %s", tasks[0].ID)
	}
}
