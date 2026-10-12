package dispatcher_test

import (
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"testing"
)

func TestCreateRowsWorkspaceOwnership(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name, want := "unassigned", orchestrator.DefaultWorkspaceSlug
		if linked {
			name, want = "assigned", "team"
		}
		t.Run(name, func(t *testing.T) {
			d := testutil.NewTestDB(t)
			p := &orchestrator.Project{ID: "repo", WorkDir: "/repo"}
			if err := orchestrator.CreateProject(d.Conn, p); err != nil {
				t.Fatal(err)
			}
			if p.Perm != "push" {
				t.Fatalf("permission = %q", p.Perm)
			}
			if linked {
				if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug) VALUES ('team')`); err != nil {
					t.Fatal(err)
				}
				if err := orchestrator.SetProjectWorkspace(d.Conn, p.ID, want); err != nil {
					t.Fatal(err)
				}
			}
			task := &orchestrator.Task{ProjectID: p.ID, WorkspaceID: "wrong", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "implement"}}
			card := &orchestrator.Task{ProjectID: p.ID, WorkspaceID: "wrong", Title: "card", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
			for _, r := range []*orchestrator.Task{task, card} {
				if err := orchestrator.CreateTask(d.Conn, r); err != nil {
					t.Fatal(err)
				}
				got, err := orchestrator.GetTask(d.Conn, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if r.WorkspaceID != want || got.WorkspaceID != want || got.ProjectID != p.ID {
					t.Fatalf("task ownership: %+v", got)
				}
			}
			job := &dispatcher.Job{ProjectID: p.ID, WorkspaceID: "wrong", TaskID: task.ID}
			session := &dispatcher.Job{ProjectID: p.ID, WorkspaceID: "wrong"}
			for _, j := range []*dispatcher.Job{job, session} {
				if err := dispatcher.CreateJob(d.Conn, j); err != nil {
					t.Fatal(err)
				}
				got, err := dispatcher.GetJob(d.Conn, j.ID)
				if err != nil {
					t.Fatal(err)
				}
				if j.WorkspaceID != want || got.WorkspaceID != want || got.ProjectID != p.ID {
					t.Fatalf("job ownership: %+v", got)
				}
			}
			run := &orchestrator.TriggerRun{ProjectID: p.ID, WorkspaceID: "wrong", TriggerName: "sweep"}
			if err := orchestrator.CreateTriggerRun(d.Conn, run); err != nil {
				t.Fatal(err)
			}
			got, err := orchestrator.LatestTriggerRun(d.Conn, p.ID, "sweep")
			if err != nil {
				t.Fatal(err)
			}
			if run.WorkspaceID != want || got.WorkspaceID != want || got.ProjectID != p.ID {
				t.Fatalf("trigger ownership: %+v", got)
			}
			for _, table := range []string{"tasks", "jobs", "trigger_runs"} {
				var n int
				if err := d.Conn.QueryRow("SELECT count(*) FROM "+table+" WHERE project_id IS NULL OR workspace_id != ?", want).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("%s wrote NULL project or incorrect workspace", table)
				}
				if _, err := d.Conn.Exec("UPDATE " + table + " SET project_id=NULL"); err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range []*orchestrator.Task{task, card} {
				got, err := orchestrator.GetTask(d.Conn, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.ProjectID != "" || got.WorkspaceID != want {
					t.Fatalf("NULL project read: %+v", got)
				}
			}
			if got, err := dispatcher.GetJob(d.Conn, job.ID); err != nil || got.ProjectID != "" || got.WorkspaceID != want {
				t.Fatalf("NULL job project read: %+v %v", got, err)
			}
			runs, err := orchestrator.ListInFlightTriggerRuns(d.Conn)
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].ProjectID != "" || runs[0].WorkspaceID != want {
				t.Fatalf("NULL trigger project read: %+v", runs)
			}
			p, err = orchestrator.GetProject(d.Conn, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if p.Perm != "push" {
				t.Fatalf("read permission: %q", p.Perm)
			}
		})
	}
}
