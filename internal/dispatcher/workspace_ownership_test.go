package dispatcher_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
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

func TestUpdateTaskReassignsWorkspaceOnProjectChange(t *testing.T) {
	for _, taskType := range []orchestrator.TaskType{orchestrator.TaskTypeExecution, orchestrator.TaskTypeCard} {
		t.Run(string(taskType), func(t *testing.T) {
			d := testutil.NewTestDB(t)
			for _, id := range []string{"first", "second", "unassigned"} {
				if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/" + id}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug) VALUES ('one'), ('two')`); err != nil {
				t.Fatal(err)
			}
			for project, workspace := range map[string]string{"first": "one", "second": "two"} {
				if err := orchestrator.SetProjectWorkspace(d.Conn, project, workspace); err != nil {
					t.Fatal(err)
				}
			}
			task := &orchestrator.Task{ProjectID: "first", Title: "task", Type: taskType}
			if taskType == orchestrator.TaskTypeExecution {
				task.Exec = &orchestrator.ExecAttrs{Behavior: "implement"}
			} else {
				task.Card = &orchestrator.CardAttrs{}
			}
			if err := orchestrator.CreateTask(d.Conn, task); err != nil {
				t.Fatal(err)
			}
			for _, change := range []struct{ project, workspace string }{{"second", "two"}, {"unassigned", orchestrator.DefaultWorkspaceSlug}, {"first", "one"}} {
				task.ProjectID, task.WorkspaceID = change.project, "wrong"
				if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
					t.Fatal(err)
				}
				got, err := orchestrator.GetTask(d.Conn, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if task.WorkspaceID != change.workspace || got.WorkspaceID != change.workspace || got.ProjectID != change.project {
					t.Fatalf("reassigned ownership: %+v, model workspace %q", got, task.WorkspaceID)
				}
			}
			if err := orchestrator.SetProjectWorkspace(d.Conn, "first", "two"); err != nil {
				t.Fatal(err)
			}
			task.Title = "renamed"
			if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
				t.Fatal(err)
			}
			got, err := orchestrator.GetTask(d.Conn, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.WorkspaceID != "one" || task.WorkspaceID != "one" || got.Title != "renamed" {
				t.Fatalf("same-project update changed ownership: %+v", got)
			}
		})
	}
}

func TestCreateJobCopiesTaskWorkspace(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "repo", WorkDir: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug) VALUES ('team')`); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "repo", "team"); err != nil {
		t.Fatal(err)
	}
	task := &orchestrator.Task{ProjectID: "repo", Title: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "implement"}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatal(err)
	}
	// Persisted task ownership can differ from current repository membership.
	if err := orchestrator.SetProjectWorkspace(d.Conn, "repo", orchestrator.DefaultWorkspaceSlug); err != nil {
		t.Fatal(err)
	}
	for _, nullableTask := range []bool{false, true} {
		if nullableTask {
			if _, err := d.Conn.Exec(`UPDATE tasks SET project_id=NULL WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct{ taskID, want string }{{task.ID, "team"}, {"", orchestrator.DefaultWorkspaceSlug}} {
			job := &dispatcher.Job{ProjectID: "repo", TaskID: tc.taskID, WorkspaceID: "wrong"}
			if err := dispatcher.CreateJob(d.Conn, job); err != nil {
				t.Fatal(err)
			}
			got, err := dispatcher.GetJob(d.Conn, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if job.WorkspaceID != tc.want || got.WorkspaceID != tc.want || got.ProjectID != "repo" {
				t.Fatalf("nullableTask=%v taskID=%q: job ownership %+v, want %q", nullableTask, tc.taskID, got, tc.want)
			}
		}
	}
}
