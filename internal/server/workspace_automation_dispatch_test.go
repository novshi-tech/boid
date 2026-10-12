package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestWorkspaceAutomationDispatchWithoutProjects(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	srv, err := New(Config{DBPath: filepath.Join(dir, "boid.db"), SocketPath: filepath.Join(dir, "boid.sock"), TLSDir: filepath.Join(dir, "tls"), HTTPAddr: "127.0.0.1:0", Backend: &fakeSandboxBackend{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	ws := orchestrator.NewWorkspaceRepository(srv.db)
	for _, slug := range []string{"team", "other"} {
		if err := ws.Save(slug, &orchestrator.WorkspaceMeta{Triggers: []orchestrator.Trigger{{Name: "sweep", Every: "1m", Run: "echo sweep"}}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	result, err := srv.workflow.SweepTriggers(context.Background(), now)
	if err != nil || len(result.Fired) != 2 {
		t.Fatalf("sweep: %+v %v", result, err)
	}
	for _, fire := range result.Fired {
		if fire.ProjectID != "" {
			t.Fatalf("project trigger: %+v", fire)
		}
		job, err := dispatcher.GetJob(srv.db, fire.JobID)
		if err != nil || job.ProjectID != "" || job.WorkspaceID != fire.WorkspaceID {
			t.Fatalf("exec job: %+v %v", job, err)
		}
		run, err := orchestrator.LatestWorkspaceTriggerRun(srv.db, fire.WorkspaceID, "sweep")
		if err != nil || run.ProjectID != "" || run.JobID != job.ID {
			t.Fatalf("run: %+v %v", run, err)
		}
	}
	executor := srv.broker.BoidExecutor.(*boidBuiltinExecutor)
	response := executor.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{WorkspaceID: "team", JobID: result.Fired[0].JobID}, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCreate, CreatePatch: json.RawMessage(`{"title":"Sweep judgment","behavior":"judge"}`)})
	if response.ExitCode != 0 {
		t.Fatal(response.Stderr)
	}
	tasks := orchestrator.NewTaskRepository(srv.db)
	listed, err := tasks.ListTasks(orchestrator.TaskFilter{WorkspaceID: "team"})
	if err != nil || len(listed) != 1 || listed[0].ProjectID != "" {
		t.Fatalf("trigger task: %+v %v", listed, err)
	}
	card, err := executor.tasks.CreateTask(context.Background(), api.CreateTaskRequest{WorkspaceID: "team", Title: "Card", InitialStatus: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := orchestrator.ListCardRequestsByCard(srv.db, card.ID)
	if err != nil || len(requests) != 1 || requests[0].CommandKey != "judge" {
		t.Fatalf("auto judge: %+v %v", requests, err)
	}
	_, err = srv.workflow.SweepQueuedCardRequests(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	requests, err = orchestrator.ListCardRequestsByCard(srv.db, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := requests[0]
	job, err := dispatcher.GetJob(srv.db, request.LauncherJobID)
	if err != nil || job.ProjectID != "" || job.WorkspaceID != "team" {
		t.Fatalf("judge launcher: %+v %v", job, err)
	}
	response = executor.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{WorkspaceID: "team", JobID: job.ID, CardID: card.ID, CardRequestID: request.ID}, &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCreate, CreatePatch: json.RawMessage(`{"title":"[judge]","behavior":"judge"}`)})
	if response.ExitCode != 0 {
		t.Fatal(response.Stderr)
	}
	request, err = orchestrator.GetCardRequest(srv.db, request.ID)
	if err != nil || request.TargetID == "" {
		t.Fatalf("judge continuation: %+v %v", request, err)
	}
	judge, err := tasks.GetTask(request.TargetID)
	if err != nil || judge.ProjectID != "" || judge.WorkspaceID != "team" || !orchestrator.IsReadonly(judge) {
		t.Fatalf("judge: %+v %v", judge, err)
	}
	var projects int
	if err := srv.db.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects); err != nil || projects != 0 {
		t.Fatalf("receiver created: %d %v", projects, err)
	}
}
