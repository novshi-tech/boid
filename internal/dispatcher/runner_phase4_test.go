package dispatcher_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunnerDispatch_RuntimeExitWithoutJobDoneFailsJob(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()
	runtimeRoot := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	taskID := "task-phase4-runtime-exit"
	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-1",
		Title:     "runtime exit fallback",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	scriptPath := filepath.Join(t.TempDir(), "runtime-exit.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: &dispatcher.LocalRuntime{RootDir: runtimeRoot},
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{scriptPath},
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

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = runner.WaitForJobCtx(waitCtx, jobID)
	if err == nil {
		t.Fatal("WaitForJobCtx error = nil, want runtime failure")
	}
	if !strings.Contains(err.Error(), "exit code") {
		t.Fatalf("WaitForJobCtx error = %v, want exit code failure", err)
	}

	job, err := dispatcher.GetJob(db.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != dispatcher.JobStatusFailed {
		t.Fatalf("job status = %q, want failed", job.Status)
	}
	if !strings.Contains(job.Output, "without boid job done") {
		t.Fatalf("job output = %q, want fallback failure message", job.Output)
	}
}
