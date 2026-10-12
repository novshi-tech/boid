package dispatcher

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDispatchWorkspaceTaskFetchOnly(t *testing.T) {
	d := newGatewayTestDB(t)
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug) VALUES ('team')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"push", "fetch", "outside"} {
		perm := "push"
		if id == "fetch" {
			perm = "fetch"
		}
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, Perm: perm, UpstreamURL: "https://github.com/owner/" + id + ".git"}); err != nil {
			t.Fatal(err)
		}
		if id != "outside" {
			if err := orchestrator.SetProjectWorkspace(d.Conn, id, "team"); err != nil {
				t.Fatal(err)
			}
		}
	}
	task := &orchestrator.Task{WorkspaceID: "team", Title: "workspace task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatal(err)
	}
	registry := gitgateway.NewRegistry()
	backend := &gwFakeBackend{}
	r := &Runner{DB: d.Conn, Projects: orchestrator.DBProjectCatalog{DB: d.Conn}, Backend: backend, BoidBinary: "/boid", GitGateway: registry}
	id, err := r.Dispatch(context.Background(), &orchestrator.JobSpec{TaskID: task.ID, Kind: orchestrator.JobKindHook, Argv: []string{"echo", "hi"}, Visibility: orchestrator.Visibility{Writable: true, Checkout: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := registry.Lookup(r.gatewayTokens[id])
	if !ok {
		t.Fatal("gateway token missing")
	}
	if len(entry.Repos) != 2 {
		t.Fatalf("repos: %v", entry.Repos)
	}
	for key, perm := range entry.Repos {
		if perm != gitgateway.PermFetch {
			t.Fatalf("%s has push: %v", key, perm)
		}
	}
	launched := backend.launched[0]
	if launched.Env["BOID_PRIMARY_REPO"] != "" {
		t.Fatalf("primary repo: %v", launched.Env)
	}
	if launched.Env["BOID_PROJECT_ID"] != "" || launched.Env["BOID_WORKSPACE_SLUG"] != "team" {
		t.Fatalf("ownership env: %v", launched.Env)
	}
	if launched.WorkDir != launched.Env["HOME"] {
		t.Fatalf("cwd: %s", launched.WorkDir)
	}
	var null bool
	var ws string
	if err := d.Conn.QueryRow(`SELECT project_id IS NULL, workspace_id FROM jobs WHERE id=?`, id).Scan(&null, &ws); err != nil {
		t.Fatal(err)
	}
	if !null || ws != "team" {
		t.Fatalf("job ownership: %v %s", null, ws)
	}
}
