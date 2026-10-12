package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestWorkspaceTaskOwnership(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, ws := range []string{"default", "team"} {
		if _, err := d.Conn.Exec(`INSERT OR IGNORE INTO workspaces (slug, created_at, updated_at) VALUES (?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []string{"", "team"} {
		task := &orchestrator.Task{WorkspaceID: ws, Title: "workspace task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatal(err)
		}
		want := ws
		if want == "" {
			want = "default"
		}
		var null bool
		var got string
		if err := d.Conn.QueryRow(`SELECT project_id IS NULL, workspace_id FROM tasks WHERE id = ?`, task.ID).Scan(&null, &got); err != nil {
			t.Fatal(err)
		}
		if !null || got != want {
			t.Fatalf("ownership: null=%v workspace=%q, want %q", null, got, want)
		}
		task.Title = "edited"
		if err := orchestrator.UpdateTask(d.Conn, task); err != nil {
			t.Fatal(err)
		}
		tasks, err := orchestrator.ListTasks(d.Conn, orchestrator.TaskFilter{WorkspaceID: want})
		if err != nil || len(tasks) != 1 {
			t.Fatalf("list: %v %v", tasks, err)
		}
		stored, err := orchestrator.GetTask(d.Conn, task.ID)
		if err != nil || stored.ProjectID != "" || stored.WorkspaceID != want {
			t.Fatalf("detail: %+v %v", stored, err)
		}
	}
}

func TestWorkspaceTaskKeysAreUniqueInWorkspace(t *testing.T) {
	d := testutil.NewTestDB(t)
	for _, ws := range []string{"default", "team"} {
		if _, err := d.Conn.Exec(`INSERT OR IGNORE INTO workspaces(slug) VALUES (?)`, ws); err != nil {
			t.Fatal(err)
		}
		task := &orchestrator.Task{WorkspaceID: ws, Title: "work", Ref: "same", IdempotencyKey: "same", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
		if err := orchestrator.CreateTask(d.Conn, task); err != nil {
			t.Fatal(err)
		}
		for _, column := range []string{"ref", "idempotency_key"} {
			_, err := d.Conn.Exec(`INSERT INTO tasks(id,type,workspace_id,title,status,behavior,traits,readonly,branch_prefix,base_branch,payload,instructions,auto_start,`+column+`) VALUES (?, 'execution', ?, 'duplicate', 'pending', '', '[]', 0, '', '', '{}', '[]', 0, 'same')`, ws+column, ws)
			if err == nil {
				t.Fatalf("duplicate %s inserted in %s", column, ws)
			}
		}
	}
}

func TestProjectKeyLookupRequiresProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	if _, err := d.Conn.Exec(`INSERT OR IGNORE INTO workspaces(slug) VALUES ('default')`); err != nil {
		t.Fatal(err)
	}
	task := &orchestrator.Task{Title: "workspace task", Ref: "stable", IdempotencyKey: "stable", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"stable", task.ID} {
		got, err := orchestrator.FindTaskByRef(d.Conn, ref, "", "")
		if err != nil || got != nil {
			t.Fatalf("unscoped ref: %+v %v", got, err)
		}
	}
	got, err := orchestrator.FindTaskByIdempotencyKey(d.Conn, "", "", "stable")
	if err != nil || got != nil {
		t.Fatalf("unscoped key: %+v %v", got, err)
	}
}
