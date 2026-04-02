package dispatcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunnerDispatch_WaitCompleteAndCleanupTrackedWindows(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()
	taskID := "task-phase6-12345678"

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-1",
		Title:     "wait and cleanup",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runtime := newStatefulRuntime()
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: runtime,
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{"/tmp/boid-phase6.sh"},
		},
	}

	jobID, err := runner.Dispatch(context.Background(), &dispatcher.DispatchPlan{
		TaskID:      taskID,
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-a.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	runtimeIDs := runtime.ActiveRuntimeIDs()
	if len(runtimeIDs) != 1 {
		t.Fatalf("dispatch should create one tracked runtime, got %v", runtimeIDs)
	}

	waitErrCh := make(chan error, 1)
	go func() {
		_, err := runner.WaitForJobCtx(context.Background(), jobID)
		waitErrCh <- err
	}()

	time.Sleep(10 * time.Millisecond)

	runner.CompleteJob(jobID, dispatcher.JobCompletionResult{
		Output:   `{"payload_patch":{"artifact":{"url":"https://example.com/artifact"}}}`,
		ExitCode: 0,
	})

	select {
	case err := <-waitErrCh:
		if err != nil {
			t.Fatalf("WaitForJobCtx: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for job completion")
	}

	runner.CleanupTaskWindow(taskID)

	if runtimeIDs = runtime.ActiveRuntimeIDs(); len(runtimeIDs) != 0 {
		t.Fatalf("cleanup should remove all tracked runtimes, got %v", runtimeIDs)
	}
}
