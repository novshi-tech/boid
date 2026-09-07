package orchestrator_test

import (
	"fmt"
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

// TestExistingTaskIDs_PastSQLiteVariableLimit pins the chunking: an id list
// well past SQLite's bound-variable ceiling must resolve, not fail with
// "too many SQL variables", and must still answer exactly for live rows
// spread across chunk boundaries.
func TestExistingTaskIDs_PastSQLiteVariableLimit(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	const total = 40000
	liveAt := []int{0, 499, 500, 1234, total - 1}
	for _, i := range liveAt {
		id := fmt.Sprintf("task-%05d", i)
		task := &orchestrator.Task{ID: id, ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatalf("create task %s: %v", id, err)
		}
	}
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		ids = append(ids, fmt.Sprintf("task-%05d", i))
	}

	exists, err := orchestrator.ExistingTaskIDs(d.Conn, ids)
	if err != nil {
		t.Fatalf("ExistingTaskIDs: %v", err)
	}
	if len(exists) != len(liveAt) {
		t.Fatalf("exists has %d entries, want %d", len(exists), len(liveAt))
	}
	for _, i := range liveAt {
		if id := fmt.Sprintf("task-%05d", i); !exists[id] {
			t.Fatalf("exists[%s] = false, want true", id)
		}
	}
}
