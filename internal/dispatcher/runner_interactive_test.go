package dispatcher_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunnerDispatch_Interactive_SetsTTY(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	taskID := "task-interactive-12345678"
	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-1",
		Title:     "interactive test",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	preparer := &fakeSandboxPreparer{
		outerPaths: []string{"/tmp/boid-interactive.sh"},
	}
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: newStatefulRuntime(),
		Sandbox: preparer,
	}

	_, err := runner.Dispatch(context.Background(), &orchestrator.DispatchRequest{
		TaskID:      taskID,
		ProjectID:   "proj-1",
		HandlerID:   "hook-interactive",
		Role:        "execution", // not "hook" or "gate" — TTY only via Interactive flag
		Interactive: true,
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(preparer.calls) != 1 {
		t.Fatalf("expected 1 sandbox call, got %d", len(preparer.calls))
	}
	if !preparer.calls[0].TTY {
		t.Fatal("expected TTY=true for Interactive plan, got false")
	}
}

func TestRunnerDispatch_Interactive_False_NoTTYForNonHookRole(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-2",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	taskID := "task-noninteractive-12345678"
	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        taskID,
		ProjectID: "proj-2",
		Title:     "non-interactive test",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	preparer := &fakeSandboxPreparer{
		outerPaths: []string{"/tmp/boid-noninteractive.sh"},
	}
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: newStatefulRuntime(),
		Sandbox: preparer,
	}

	_, err := runner.Dispatch(context.Background(), &orchestrator.DispatchRequest{
		TaskID:      taskID,
		ProjectID:   "proj-2",
		HandlerID:   "hook-noninteractive",
		Role:        "execution",
		Interactive: false,
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(preparer.calls) != 1 {
		t.Fatalf("expected 1 sandbox call, got %d", len(preparer.calls))
	}
	if preparer.calls[0].TTY {
		t.Fatal("expected TTY=false for non-Interactive execution plan, got true")
	}
}
