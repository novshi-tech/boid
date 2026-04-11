package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		d.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCleanOrphanRuntimes_RemovesOrphans(t *testing.T) {
	d := openTestDB(t)
	runtimesDir := t.TempDir()

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev", Status: orchestrator.TaskStatusDone}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// DB に job 行がある runtime（残すべき）
	const knownRuntimeID = "runtime-known"
	job := &dispatcher.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HandlerID: "handler",
		RuntimeID: knownRuntimeID,
		Status:    dispatcher.JobStatusCompleted,
	}
	if err := dispatcher.CreateJob(d.Conn, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	knownDir := filepath.Join(runtimesDir, knownRuntimeID)
	if err := os.MkdirAll(knownDir, 0o755); err != nil {
		t.Fatalf("mkdir known dir: %v", err)
	}

	// DB に job 行がない orphan runtime（削除されるべき）
	const orphanRuntimeID = "runtime-orphan"
	orphanDir := filepath.Join(runtimesDir, orphanRuntimeID)
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatalf("mkdir orphan dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "transcript.log"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cleanOrphanRuntimes(runtimesDir, d.Conn)

	// orphan は削除されている
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan runtime dir should be removed, err: %v", err)
	}
	// known は残っている
	if _, err := os.Stat(knownDir); err != nil {
		t.Errorf("known runtime dir should still exist: %v", err)
	}
}

func TestCleanOrphanRuntimes_NonexistentDir(t *testing.T) {
	d := openTestDB(t)
	// 存在しないディレクトリを渡してもパニックしないことを確認
	cleanOrphanRuntimes("/nonexistent/path/runtimes", d.Conn)
}
