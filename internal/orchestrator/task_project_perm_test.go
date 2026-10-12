package orchestrator_test

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"strings"
	"testing"
)

func TestTaskStore_PrimaryProjectPermission(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, perm := range []string{"fetch", "push"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: perm, Perm: perm}); err != nil {
			t.Fatal(err)
		}
		task := &orchestrator.Task{ProjectID: perm, Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
		err := orchestrator.CreateTask(d.Conn, task)
		if perm == "fetch" {
			if err == nil || !strings.Contains(err.Error(), "perm=fetch") {
				t.Fatalf("expected fetch rejection, got %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		task.ProjectID = "fetch"
		if err := orchestrator.UpdateTask(d.Conn, task); err == nil {
			t.Fatal("fetch reassignment accepted")
		}
		stored, err := orchestrator.GetTask(d.Conn, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ProjectID != "push" {
			t.Fatal(stored.ProjectID)
		}
		if err := orchestrator.SetProjectPerm(d.Conn, "push", "fetch"); err != nil {
			t.Fatal(err)
		}
		stored.Title = "still editable"
		if err := orchestrator.UpdateTask(d.Conn, stored); err != nil {
			t.Fatal(err)
		}
	}
}
