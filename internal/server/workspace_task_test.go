package server

import (
	"context"
	"database/sql"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestSandboxCreateWorkspaceChild(t *testing.T) {
	conn := newBoidExecutorTestDB(t)
	repo := orchestrator.NewWorkspaceRepository(conn.(*sql.DB))
	if err := repo.Save("team", &orchestrator.WorkspaceMeta{DefaultTaskBehavior: "work", TaskBehaviors: map[string]orchestrator.TaskBehavior{"work": {}}}); err != nil {
		t.Fatal(err)
	}
	ws := orchestrator.NewWorkspaceStore("")
	ws.SetRepository(repo)
	meta := orchestrator.NewProjectStore()
	meta.SetWorkspaceStore(ws)
	tasks := orchestrator.NewTaskRepository(conn)
	executor := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: tasks, Meta: meta}}
	for _, project := range []string{"primary", ""} {
		ctx := sandbox.TokenContext{ProjectID: project, WorkspaceID: "team"}
		response := executor.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCreate, CreatePatch: []byte(`{"title":"child","no_project":true}`)})
		if response.ExitCode != 0 {
			t.Fatal(response.Stderr)
		}
	}
	listed, err := tasks.ListTasks(orchestrator.TaskFilter{WorkspaceID: "team"})
	if err != nil || len(listed) != 2 {
		t.Fatalf("children: %v %v", listed, err)
	}
	for _, task := range listed {
		if task.ProjectID != "" {
			t.Fatal(task.ProjectID)
		}
	}
	for _, ctx := range []sandbox.TokenContext{{WorkspaceID: "team"}, {ProjectID: "primary", WorkspaceID: "team"}, {WorkspaceID: "other"}} {
		response := executor.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskGet, TaskID: listed[0].ID, TaskField: "title"})
		want := 0
		if ctx.WorkspaceID == "other" {
			want = 1
		}
		if response.ExitCode != want {
			t.Fatalf("read scope %+v: %+v", ctx, response)
		}
	}
}

func TestSandboxUpdateProjectScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctx     sandbox.TokenContext
		project string
		want    int
	}{
		{"foreign", sandbox.TokenContext{WorkspaceID: "team", AllowedProjectIDs: []string{"local"}}, "foreign", 1},
		{"workspace job", sandbox.TokenContext{WorkspaceID: "team", AllowedProjectIDs: []string{"local"}}, "local", 0},
		{"project job", sandbox.TokenContext{ProjectID: "local", WorkspaceID: "team"}, "local", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &capturingTaskStore{created: []*orchestrator.Task{{ID: "task", WorkspaceID: "team", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusPending, Exec: &orchestrator.ExecAttrs{}}}}
			executor := &boidBuiltinExecutor{tasks: &api.TaskAppService{Tasks: store, Meta: executorMetaStub{meta: &orchestrator.ProjectMeta{}}}}
			response := executor.ExecuteBoidBuiltin(context.Background(), tc.ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskUpdate, TaskID: "task", UpdatePatch: []byte(`{"project_id":"` + tc.project + `"}`)})
			if response.ExitCode != tc.want {
				t.Fatalf("response: %+v", response)
			}
			if tc.want != 0 && len(store.updated) != 0 {
				t.Fatal("forbidden update reached store")
			}
		})
	}
}
