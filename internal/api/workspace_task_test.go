package api

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestCreateWorkspaceTaskLifecycle(t *testing.T) {
	svc, tasks, conn := newSeamService(t)
	repo := orchestrator.NewWorkspaceRepository(conn)
	wsStore := orchestrator.NewWorkspaceStore("")
	wsStore.SetRepository(repo)
	meta := orchestrator.NewProjectStore()
	meta.SetWorkspaceStore(wsStore)
	svc.Meta = meta
	svc.Workflow = &TaskWorkflowService{Tasks: tasks, Meta: meta, Tx: svc.Tx, Jobs: &stubJobStore{}}
	svc.Notify = &capturingNotifier{}
	for _, ws := range []string{"default", "team"} {
		if err := repo.Save(ws, &orchestrator.WorkspaceMeta{DefaultTaskBehavior: "work", Env: map[string]string{"WORKSPACE": "yes"}, TaskBehaviors: map[string]orchestrator.TaskBehavior{"work": {Readonly: boolPtr(false)}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ws := range []string{"", "team"} {
		task, err := svc.CreateTask(context.Background(), CreateTaskRequest{Title: "work", WorkspaceID: ws, Ref: "stable", IdempotencyKey: "stable"})
		if err != nil {
			t.Fatal(err)
		}
		want := ws
		if want == "" {
			want = "default"
		}
		if task.ProjectID != "" || task.WorkspaceID != want || task.Exec.Behavior != "work" {
			t.Fatalf("task: %+v", task)
		}
		retry, err := svc.CreateTask(context.Background(), CreateTaskRequest{Title: "retry", WorkspaceID: ws, Ref: "stable"})
		if err != nil || retry.ID != task.ID {
			t.Fatalf("retry: %+v %v", retry, err)
		}
		retryKey, err := svc.CreateTask(context.Background(), CreateTaskRequest{Title: "retry", WorkspaceID: ws, IdempotencyKey: "stable"})
		if err != nil || retryKey.ID != task.ID {
			t.Fatalf("key retry: %+v %v", retryKey, err)
		}
		listed, err := tasks.ListTasks(orchestrator.TaskFilter{WorkspaceID: want, Behavior: "work", Title: "work", Status: "pending"})
		if err != nil || len(listed) != 1 {
			t.Fatalf("combined filters: %v %v", listed, err)
		}
		svc.Jobs = &stubJobStore{}
		detail, err := svc.GetTaskDetail(task.ID)
		if err != nil || detail.Task.WorkspaceID != want {
			t.Fatalf("detail: %+v %v", detail, err)
		}
		resolved, err := orchestrator.TaskMeta(context.Background(), meta, task)
		if err != nil || resolved.SecretNamespace != want || resolved.TaskBehaviors["work"].Env["WORKSPACE"] != "yes" {
			t.Fatalf("meta: %+v %v", resolved, err)
		}
		planner := &orchestrator.DispatchPlanner{Meta: meta, Hydrator: meta, Projects: orchestrator.DBProjectCatalog{DB: conn}, Tasks: tasks}
		spec, _, err := planner.PlanHook(&orchestrator.HookFireEvent{TaskID: task.ID, Hook: orchestrator.Hook{ID: "run", Command: "echo hi"}})
		if err != nil || spec.Visibility.Checkout || spec.ProjectID != "" {
			t.Fatalf("plan: %+v %v", spec, err)
		}
		result, err := svc.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil || result.Task.Status != orchestrator.TaskStatusExecuting {
			t.Fatalf("start: %+v %v", result, err)
		}
		if err := svc.NotifyTask(context.Background(), task.ID, "done", "", "", "", "complete", ""); err != nil {
			t.Fatal(err)
		}
		actions, err := tasks.ListActionsByTask(task.ID)
		if err != nil || len(actions) != 2 || actions[1].Type != "done_request" {
			t.Fatalf("timeline: %v %v", actions, err)
		}
		task.Status = orchestrator.TaskStatusDone
		if err := tasks.UpdateTask(task); err != nil {
			t.Fatal(err)
		}
	}
	result, err := orchestrator.GCTasks(conn, []string{"done"}, time.Duration(0), false)
	if err != nil || result == nil || result.Tasks != 2 {
		t.Fatalf("GC: %+v %v", result, err)
	}
}

func TestCreateTaskWorkspaceConflict(t *testing.T) {
	svc, tasks, conn := newSeamService(t)
	svc.Projects = orchestrator.DBProjectCatalog{DB: conn}
	for _, req := range []CreateTaskRequest{
		{ProjectID: "proj-1", WorkspaceID: "other", Title: "conflict"},
		{ProjectID: "proj-1", NoProject: true, Title: "conflict"},
	} {
		if _, err := svc.CreateTask(context.Background(), req); err == nil {
			t.Fatal("conflicting ownership accepted")
		}
	}
	listed, err := tasks.ListTasks(orchestrator.TaskFilter{})
	if err != nil || len(listed) != 0 {
		t.Fatalf("created rows: %v %v", listed, err)
	}
}

func TestWorkspaceHookDisplayName(t *testing.T) {
	_, _, conn := newSeamService(t)
	repo := orchestrator.NewWorkspaceRepository(conn)
	if err := repo.Save("team", &orchestrator.WorkspaceMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"work": {Hooks: []orchestrator.Hook{{ID: "run", Name: "Workspace worker", Command: "echo hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	ws := orchestrator.NewWorkspaceStore("")
	ws.SetRepository(repo)
	meta := orchestrator.NewProjectStore()
	meta.SetWorkspaceStore(ws)
	job := &Job{WorkspaceID: "team", Role: "hook", HandlerID: "run"}
	enrichJobDisplayName(job, "work", meta)
	if job.DisplayName != "Workspace worker" {
		t.Fatalf("display name: %q", job.DisplayName)
	}
}
