package job_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunner_Execute(t *testing.T) {
	db := testutil.NewTestDB(t)
	store := project.NewStore(nil)
	mockTmux := testutil.NewMockTmux()

	// Create project in DB
	proj := &model.Project{ID: "proj-1", WorkDir: t.TempDir()}
	if err := db.CreateProject(proj); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Set meta in store
	store.Set("proj-1", &model.ProjectMeta{
		ID:   "proj-1",
		Name: "Test Project",
	})

	// Create task in DB
	task := &model.Task{
		ProjectID: "proj-1",
		Title:     "Test Task",
		Behavior:  "implementation",
	}
	if err := db.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runner := &job.Runner{
		DB:           db,
		Store:        store,
		Tmux:         mockTmux,
		TmuxSession:  "boid-test",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/tmp/test-boid.sock",
	}

	event := &model.HookFireEvent{
		EventID:   "evt-001",
		TaskID:    task.ID,
		ProjectID: "proj-1",
		Hook: model.Hook{
			ID:         "run-agent",
			On:         "executing",
			ScriptPath: "/some/path/run-agent.sh",
		},
	}

	if err := runner.Execute(context.Background(), event); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify tmux session was created
	if !mockTmux.HasSession("boid-test") {
		t.Error("expected tmux session 'boid-test' to exist")
	}

	// Verify a window was created in the session
	windows, err := mockTmux.ListWindows("boid-test")
	if err != nil {
		t.Fatalf("list windows: %v", err)
	}
	if len(windows) == 0 {
		t.Error("expected at least one tmux window")
	}

	// Verify job was created in DB
	jobs, err := db.ListJobsByTask(task.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ProjectID != "proj-1" {
		t.Errorf("job project_id = %q, want %q", jobs[0].ProjectID, "proj-1")
	}
	if jobs[0].HandlerID != "run-agent" {
		t.Errorf("job handler_id = %q, want %q", jobs[0].HandlerID, "run-agent")
	}
	if jobs[0].Status != model.JobStatusRunning {
		t.Errorf("job status = %q, want %q", jobs[0].Status, model.JobStatusRunning)
	}
}

func TestRunner_WaitForJob_CompleteJob(t *testing.T) {
	runner := &job.Runner{}

	// Register a wait channel
	ch := runner.WaitForJob("job-123")

	// Complete the job in a goroutine
	go func() {
		runner.CompleteJob("job-123", job.JobCompletionResult{
			Output:   `{"payload_patch":{"agent_prompt":"done"}}`,
			ExitCode: 0,
		})
	}()

	// Wait should receive the completion
	result := <-ch
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if result.Output != `{"payload_patch":{"agent_prompt":"done"}}` {
		t.Errorf("output = %q", result.Output)
	}
}

func TestRunner_WaitForJobCtx_Success(t *testing.T) {
	runner := &job.Runner{}

	go func() {
		runner.CompleteJob("job-ctx-1", job.JobCompletionResult{
			Output:   `{"payload_patch":{}}`,
			ExitCode: 0,
		})
	}()

	result, err := runner.WaitForJobCtx(context.Background(), "job-ctx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestRunner_WaitForJobCtx_Timeout(t *testing.T) {
	runner := &job.Runner{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel

	_, err := runner.WaitForJobCtx(ctx, "job-timeout")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunner_WaitForJobCtx_FailedJob(t *testing.T) {
	runner := &job.Runner{}

	go func() {
		runner.CompleteJob("job-fail", job.JobCompletionResult{
			Output:   "error",
			ExitCode: 1,
		})
	}()

	_, err := runner.WaitForJobCtx(context.Background(), "job-fail")
	if err == nil {
		t.Fatal("expected error for failed job")
	}
}

func TestRunner_WaitForJob_UnknownJobDropped(t *testing.T) {
	runner := &job.Runner{}

	// CompleteJob for unknown job should not panic
	runner.CompleteJob("nonexistent", job.JobCompletionResult{ExitCode: 0})
}

func TestRunner_Execute_NoScriptPath_Error(t *testing.T) {
	db := testutil.NewTestDB(t)
	store := project.NewStore(nil)
	mockTmux := testutil.NewMockTmux()

	runner := &job.Runner{
		DB:           db,
		Store:        store,
		Tmux:         mockTmux,
		TmuxSession:  "boid-test",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/tmp/test-boid.sock",
	}

	event := &model.HookFireEvent{
		EventID:   "evt-002",
		TaskID:    "task-12345678",
		ProjectID: "proj-1",
		Hook: model.Hook{
			ID:         "run-agent",
			On:         "executing",
			ScriptPath: "", // empty script path
		},
	}

	err := runner.Execute(context.Background(), event)
	if err == nil {
		t.Fatal("expected error for empty ScriptPath, got nil")
	}
}
