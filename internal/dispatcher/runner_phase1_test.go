package dispatcher_test

import (
	"context"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func cleanupSandboxScripts(t *testing.T, jobIDs ...string) {
	t.Helper()
	for _, jobID := range jobIDs {
		for _, suffix := range []string{"-inner.sh", "-setup.sh", "-outer.sh"} {
			_ = os.Remove("/tmp/boid-" + jobID + suffix)
		}
	}
}

func TestRunnerDispatch_StartsJobRuntimeAndPersistsMetadata(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	taskID := "task-12345678-abcd-efgh-ijkl-mnopqrstuvwx"
	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-1",
		Title:     "parallel hooks",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runtime := newStatefulRuntime()
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: runtime,
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{"/tmp/boid-hook-a.sh", "/tmp/boid-hook-b.sh"},
		},
	}

	planA := &orchestrator.JobSpec{
		TaskID:      taskID,
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        orchestrator.RoleHook,
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-a.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	}
	planB := &orchestrator.JobSpec{
		TaskID:      taskID,
		ProjectID:   "proj-1",
		HandlerID:   "hook-b",
		Role:        orchestrator.RoleHook,
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-b.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	}

	jobID1, err := runner.Dispatch(context.Background(), planA)
	if err != nil {
		t.Fatalf("dispatch hook-a: %v", err)
	}
	jobID2, err := runner.Dispatch(context.Background(), planB)
	if err != nil {
		t.Fatalf("dispatch hook-b: %v", err)
	}

	runtimeIDs := runtime.ActiveRuntimeIDs()
	if len(runtimeIDs) != 2 {
		t.Fatalf("expected 2 active runtimes, got %v", runtimeIDs)
	}

	spec1, ok := runtime.StartSpec(runtimeIDs[0])
	if !ok {
		t.Fatalf("missing runtime spec for %s", runtimeIDs[0])
	}
	spec2, ok := runtime.StartSpec(runtimeIDs[1])
	if !ok {
		t.Fatalf("missing runtime spec for %s", runtimeIDs[1])
	}
	if spec1.JobID == spec2.JobID {
		t.Fatalf("runtime start specs should target distinct jobs, got %+v and %+v", spec1, spec2)
	}

	job1, err := dispatcher.GetJob(db.Conn, jobID1)
	if err != nil {
		t.Fatalf("get job1: %v", err)
	}
	if job1.RuntimeID == "" {
		t.Fatal("job1 runtime_id is empty")
	}
	if !job1.Interactive {
		t.Fatal("job1 interactive = false, want true")
	}
	if !job1.TTY {
		t.Fatal("job1 tty = false, want true")
	}

	job2, err := dispatcher.GetJob(db.Conn, jobID2)
	if err != nil {
		t.Fatalf("get job2: %v", err)
	}
	if job2.RuntimeID == "" {
		t.Fatal("job2 runtime_id is empty")
	}
	if job1.RuntimeID == job2.RuntimeID {
		t.Fatalf("jobs should have distinct runtime ids, got %q", job1.RuntimeID)
	}
}

func TestRunnerCleanupTaskWindow_StopsAllTrackedRuntimes(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()
	taskID := "task-cleanup-12345678-abcd-efgh"

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-1",
		Title:     "cleanup hooks",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runtime := newStatefulRuntime()
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: runtime,
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{"/tmp/boid-cleanup-a.sh", "/tmp/boid-cleanup-b.sh"},
		},
	}

	for _, handlerID := range []string{"hook-a", "hook-b"} {
		_, err := runner.Dispatch(context.Background(), &orchestrator.JobSpec{
			TaskID:      taskID,
			ProjectID:   "proj-1",
			HandlerID:   handlerID,
			Role:        orchestrator.RoleHook,
			ProjectDir:  projectDir,
			HomeDir:     projectDir,
			HookScript:  handlerID + ".sh",
			BoidBinary:  "/bin/true",
			PayloadJSON: `{}`,
		})
		if err != nil {
			t.Fatalf("dispatch %s: %v", handlerID, err)
		}
	}

	runner.CleanupTaskWindow(taskID)

	if active := runtime.ActiveRuntimeIDs(); len(active) != 0 {
		t.Fatalf("cleanup should remove all tracked runtimes, got %v", active)
	}
	if stopped := runtime.StoppedRuntimeIDs(); len(stopped) != 2 {
		t.Fatalf("cleanup should stop 2 runtimes, got %v", stopped)
	}
}
