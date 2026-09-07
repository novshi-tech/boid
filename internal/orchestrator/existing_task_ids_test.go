package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestExistingTaskIDs_MixOfLiveMissingAndDuplicateIDs(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	live := &orchestrator.Task{ID: "task-live", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, live); err != nil {
		t.Fatalf("create task: %v", err)
	}

	exists, err := orchestrator.ExistingTaskIDs(d.Conn, []string{"task-live", "task-missing", "task-live", ""})
	if err != nil {
		t.Fatalf("ExistingTaskIDs: %v", err)
	}
	if !exists["task-live"] {
		t.Fatalf("exists[task-live] = false, want true")
	}
	if exists["task-missing"] {
		t.Fatalf("exists[task-missing] = true, want false")
	}
	if exists[""] {
		t.Fatalf("exists[\"\"] = true, want false")
	}
	if len(exists) != 1 {
		t.Fatalf("exists = %+v, want exactly one entry", exists)
	}
}

func TestExistingTaskIDs_EmptyInput(t *testing.T) {
	d := testutil.NewTestDB(t)
	exists, err := orchestrator.ExistingTaskIDs(d.Conn, nil)
	if err != nil {
		t.Fatalf("ExistingTaskIDs: %v", err)
	}
	if len(exists) != 0 {
		t.Fatalf("exists = %+v, want empty", exists)
	}
}
