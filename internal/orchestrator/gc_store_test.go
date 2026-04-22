package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			if strings.Contains(string(out), "cwd does not exist") {
				t.Skip("git not available outside worktree in this environment")
			}
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
	result, err := gcStore.GC(0, false)
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
	result, err := orchestrator.GCTasks(d.Conn, []string{}, 0, false)
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
	result, err := gcStore.GC(30*24*time.Hour, true)
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
	result, err = gcStore.GC(30*24*time.Hour, false)
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
	result, err := gcStore.GC(0, false)
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
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin, "")

	result, err := gcStore.GC(0, false)
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
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin, "")

	result, err := gcStore.GC(0, false)
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
	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, resolveProjectDir, gcTestGitBin, "")

	// dry-run: ディスク操作はスキップされる
	result, err := gcStore.GC(0, true)
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

func TestGCTasks_AllDone(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	task1 := &orchestrator.Task{ProjectID: "proj-1", Title: "Done 1", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}

	task2 := &orchestrator.Task{ProjectID: "proj-1", Title: "Done 2", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	pendingTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Pending", Behavior: "dev"}
	if err := orchestrator.CreateTask(d.Conn, pendingTask); err != nil {
		t.Fatalf("create pending task: %v", err)
	}

	result, err := orchestrator.GCTasks(d.Conn, []string{"done", "aborted"}, 0, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Tasks != 2 {
		t.Fatalf("expected 2 deleted tasks, got %d", result.Tasks)
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

// makeRuntimeDir は runtimesDir 配下に fake の runtime ディレクトリを作成する。
func makeRuntimeDir(t *testing.T, runtimesDir, runtimeID string) string {
	t.Helper()
	dir := filepath.Join(runtimesDir, runtimeID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	// transcript.log を模したファイルを追加
	if err := os.WriteFile(filepath.Join(dir, "transcript.log"), []byte("log content"), 0o644); err != nil {
		t.Fatalf("write transcript.log: %v", err)
	}
	return dir
}

func TestGC_SandboxTmpCleanup(t *testing.T) {
	d := testutil.NewTestDB(t)

	tmpDir := t.TempDir()
	oldTime := time.Now().Add(-48 * time.Hour)

	oldScript := filepath.Join(tmpDir, "boid-oldjob-inner.sh")
	if err := os.WriteFile(oldScript, []byte("x"), 0o755); err != nil {
		t.Fatalf("write old script: %v", err)
	}
	if err := os.Chtimes(oldScript, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	freshScript := filepath.Join(tmpDir, "boid-fresh-inner.sh")
	if err := os.WriteFile(freshScript, []byte("x"), 0o755); err != nil {
		t.Fatalf("write fresh script: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn).WithSandboxTmpDir(tmpDir)
	result, err := gcStore.GC(24*time.Hour, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.SandboxTmp != 1 {
		t.Errorf("SandboxTmp = %d, want 1", result.SandboxTmp)
	}
	if _, err := os.Stat(oldScript); !os.IsNotExist(err) {
		t.Errorf("old script should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(freshScript); err != nil {
		t.Errorf("fresh script should remain: %v", err)
	}
}

func TestGC_SandboxTmpCleanup_DryRunSkipsRemoval(t *testing.T) {
	d := testutil.NewTestDB(t)

	tmpDir := t.TempDir()
	oldTime := time.Now().Add(-48 * time.Hour)
	oldScript := filepath.Join(tmpDir, "boid-oldjob-outer.sh")
	if err := os.WriteFile(oldScript, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(oldScript, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	gcStore := orchestrator.NewTaskGCStore(d.Conn).WithSandboxTmpDir(tmpDir)
	result, err := gcStore.GC(24*time.Hour, true)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	if result.SandboxTmp != 0 {
		t.Errorf("dry-run: SandboxTmp = %d, want 0", result.SandboxTmp)
	}
	if _, err := os.Stat(oldScript); err != nil {
		t.Errorf("dry-run: old script should remain: %v", err)
	}
}

func TestGC_RuntimesDirCleanup(t *testing.T) {
	d := testutil.NewTestDB(t)
	runtimesDir := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// runtime_id を持つ job を作成
	const runtimeID = "runtime-abc123"
	job := &dispatcher.Job{
		TaskID:    doneTask.ID,
		ProjectID: "proj-1",
		HandlerID: "test-handler",
		RuntimeID: runtimeID,
		Status:    dispatcher.JobStatusCompleted,
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	runtimeDir := makeRuntimeDir(t, runtimesDir, runtimeID)

	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, nil, "", runtimesDir)
	result, err := gcStore.GC(0, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Runtimes != 1 {
		t.Fatalf("expected 1 runtime deleted, got %d", result.Runtimes)
	}

	// runtime ディレクトリが削除されていることを確認
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Errorf("runtime dir should be removed after GC, err: %v", err)
	}
}

func TestGC_RuntimesDirCleanup_DryRun(t *testing.T) {
	d := testutil.NewTestDB(t)
	runtimesDir := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	doneTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Done Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, doneTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	const runtimeID = "runtime-dryrun"
	job := &dispatcher.Job{
		TaskID:    doneTask.ID,
		ProjectID: "proj-1",
		HandlerID: "test-handler",
		RuntimeID: runtimeID,
		Status:    dispatcher.JobStatusCompleted,
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	runtimeDir := makeRuntimeDir(t, runtimesDir, runtimeID)

	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, nil, "", runtimesDir)
	result, err := gcStore.GC(0, true)
	if err != nil {
		t.Fatalf("gc dry-run: %v", err)
	}
	// dry-run は GCTasks が DB の distinct runtime_id をカウントする
	if result.Runtimes != 1 {
		t.Fatalf("dry-run: expected 1 runtime counted, got %d", result.Runtimes)
	}

	// dry-run なので runtime ディレクトリは残っている
	if _, err := os.Stat(runtimeDir); err != nil {
		t.Errorf("runtime dir should still exist after dry-run GC: %v", err)
	}
}

func TestGC_RuntimesDirCleanup_OlderThanFilter(t *testing.T) {
	d := testutil.NewTestDB(t)
	runtimesDir := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// 古いタスク（60日前）
	oldTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Old Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, oldTask); err != nil {
		t.Fatalf("create old task: %v", err)
	}
	sixtyDaysAgo := time.Now().UTC().Add(-60 * 24 * time.Hour)
	if _, err := d.Conn.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, sixtyDaysAgo, oldTask.ID); err != nil {
		t.Fatalf("update updated_at: %v", err)
	}

	// 新しいタスク（今）
	recentTask := &orchestrator.Task{ProjectID: "proj-1", Title: "Recent Done", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, recentTask); err != nil {
		t.Fatalf("create recent task: %v", err)
	}

	const oldRuntimeID = "runtime-old"
	const recentRuntimeID = "runtime-recent"

	oldJob := &dispatcher.Job{TaskID: oldTask.ID, ProjectID: "proj-1", HandlerID: "h", RuntimeID: oldRuntimeID, Status: dispatcher.JobStatusCompleted}
	if err := dispatcher.CreateJob(d.Conn, oldJob); err != nil {
		t.Fatalf("create old job: %v", err)
	}
	recentJob := &dispatcher.Job{TaskID: recentTask.ID, ProjectID: "proj-1", HandlerID: "h", RuntimeID: recentRuntimeID, Status: dispatcher.JobStatusCompleted}
	if err := dispatcher.CreateJob(d.Conn, recentJob); err != nil {
		t.Fatalf("create recent job: %v", err)
	}

	oldRuntimeDir := makeRuntimeDir(t, runtimesDir, oldRuntimeID)
	recentRuntimeDir := makeRuntimeDir(t, runtimesDir, recentRuntimeID)

	gcStore := orchestrator.NewTaskGCStoreWithWorktree(d.Conn, nil, "", runtimesDir)
	result, err := gcStore.GC(30*24*time.Hour, false)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if result.Runtimes != 1 {
		t.Fatalf("expected 1 runtime deleted, got %d", result.Runtimes)
	}

	// 古い runtime は削除されている
	if _, err := os.Stat(oldRuntimeDir); !os.IsNotExist(err) {
		t.Errorf("old runtime dir should be removed, err: %v", err)
	}
	// 新しい runtime は残っている
	if _, err := os.Stat(recentRuntimeDir); err != nil {
		t.Errorf("recent runtime dir should still exist: %v", err)
	}
}
