package orchestrator_test

// TestTaskStatusesByIDs_* pins the batched status lookup PR-5c's list
// activity state needs for a card's sole dispatched work child: many task
// ids resolved to their live status in one query, mirroring
// ExistingTaskIDs's own chunking contract.

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestTaskStatusesByIDs_MixOfStatusesMissingAndDuplicates(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	pending := &orchestrator.Task{ID: "task-pending", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, pending); err != nil {
		t.Fatalf("create pending task: %v", err)
	}
	executing := &orchestrator.Task{ID: "task-executing", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, executing); err != nil {
		t.Fatalf("create executing task: %v", err)
	}

	got, err := orchestrator.TaskStatusesByIDs(d.Conn, []string{"task-pending", "task-executing", "task-pending", "task-missing", ""})
	if err != nil {
		t.Fatalf("TaskStatusesByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2, got %+v", len(got), got)
	}
	if got["task-pending"] != orchestrator.TaskStatusPending {
		t.Errorf("task-pending status = %q, want pending", got["task-pending"])
	}
	if got["task-executing"] != orchestrator.TaskStatusExecuting {
		t.Errorf("task-executing status = %q, want executing", got["task-executing"])
	}
	if _, ok := got["task-missing"]; ok {
		t.Errorf("task-missing must be absent, got %+v", got)
	}
}

func TestTaskStatusesByIDs_EmptyInput_NoRowsNoError(t *testing.T) {
	d := testutil.NewTestDB(t)
	got, err := orchestrator.TaskStatusesByIDs(d.Conn, nil)
	if err != nil {
		t.Fatalf("TaskStatusesByIDs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}
