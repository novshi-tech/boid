package server

// The wire end of the card state records: the service reads the writer off
// ctx, and ExecuteBoidBuiltin is the only place that puts it there. These pin
// that half — a bare goCtx at any of the three ops would record a sandbox
// write as if a person had made it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

func newCardActorExecutor(t *testing.T) (*boidBuiltinExecutor, *orchestrator.TaskRepository, *sql.DB) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(d.Conn)
	svc := &api.TaskAppService{
		Tasks:      repo,
		Actions:    repo,
		Identities: repo,
		Tx:         apiTransactor{db: d.Conn},
	}
	return &boidBuiltinExecutor{tasks: svc}, repo, d.Conn
}

func seedActorCard(t *testing.T, repo *orchestrator.TaskRepository) *orchestrator.Task {
	t.Helper()
	card := &orchestrator.Task{
		ProjectID: "proj-1", Type: orchestrator.TaskTypeCard,
		Title: "a card", Description: "before", Status: orchestrator.TaskStatusParked,
		Card: &orchestrator.CardAttrs{},
	}
	if err := repo.CreateTask(card); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return card
}

func onlyAction(t *testing.T, repo *orchestrator.TaskRepository, taskID string) *orchestrator.Action {
	t.Helper()
	actions, err := repo.ListActionsByTask(taskID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1: %+v", len(actions), actions)
	}
	return actions[0]
}

func sandboxCtx() sandbox.TokenContext {
	return sandbox.TokenContext{
		TaskID: "t-writer", ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"},
	}
}

func TestBoidExecutor_TaskUpdate_RecordsTheWritingTaskAsActor(t *testing.T) {
	exec, repo, _ := newCardActorExecutor(t)
	card := seedActorCard(t, repo)

	resp := exec.ExecuteBoidBuiltin(context.Background(), sandboxCtx(), &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskUpdate, TaskID: card.ID,
		UpdatePatch: json.RawMessage(`{"description":"after"}`),
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit = %d (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	got := onlyAction(t, repo, card.ID)
	if got.Type != orchestrator.ActionTypeCardEdited {
		t.Fatalf("type = %q, want %q", got.Type, orchestrator.ActionTypeCardEdited)
	}
	if got.Actor != orchestrator.ActorTask("t-writer") {
		t.Errorf("actor = %q, want the writing task", got.Actor)
	}
}

func TestBoidExecutor_IdentityLinkAndUnlink_RecordTheWritingTaskAsActor(t *testing.T) {
	exec, repo, _ := newCardActorExecutor(t)
	card := seedActorCard(t, repo)

	resp := exec.ExecuteBoidBuiltin(context.Background(), sandboxCtx(), &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskIdentityLink, ProjectID: "proj-1", Identity: "jira:ACTOR-1", TaskID: card.ID,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("link exit = %d (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if got := onlyAction(t, repo, card.ID); got.Actor != orchestrator.ActorTask("t-writer") {
		t.Errorf("link actor = %q, want the writing task", got.Actor)
	}

	resp = exec.ExecuteBoidBuiltin(context.Background(), sandboxCtx(), &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskIdentityUnlink, ProjectID: "proj-1", Identity: "jira:ACTOR-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("unlink exit = %d (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	actions, err := repo.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2: %+v", len(actions), actions)
	}
	if actions[1].Type != orchestrator.ActionTypeIdentityUnlinked {
		t.Fatalf("second type = %q, want %q", actions[1].Type, orchestrator.ActionTypeIdentityUnlinked)
	}
	if actions[1].Actor != orchestrator.ActorTask("t-writer") {
		t.Errorf("unlink actor = %q, want the writing task", actions[1].Actor)
	}
}

// TestBoidExecutor_TaskCreateAutoStart_RecordsTheWritingTaskAsActor: the start
// an auto_start create fires is the sandbox's action too. Minting ActorHuman
// there would hand a sandbox the one actor card transitions are gated on.
func TestBoidExecutor_TaskCreateAutoStart_RecordsTheWritingTaskAsActor(t *testing.T) {
	exec, repo, conn := newCardActorExecutor(t)
	workflow := &api.TaskWorkflowService{
		Tasks: repo, Actions: repo, Tx: apiTransactor{db: conn},
		Meta: stubExecutorMetaStore{},
	}
	exec.tasks.Workflow = workflow

	task, err := exec.tasks.CreateTask(
		orchestrator.WithActor(context.Background(), orchestrator.ActorTask("t-writer")),
		api.CreateTaskRequest{ProjectID: "proj-1", Title: "t", Behavior: "dev", AutoStart: true},
	)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	actions, err := repo.ListActionsByTask(task.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	var start *orchestrator.Action
	for _, a := range actions {
		if a.Type == "start" {
			start = a
		}
	}
	if start == nil {
		t.Fatalf("no start action was recorded: %+v", actions)
	}
	if start.Actor != orchestrator.ActorTask("t-writer") {
		t.Errorf("start actor = %q, want the writing task", start.Actor)
	}
}

// stubExecutorMetaStore is the minimum ProjectMeta the auto_start path reads.
type stubExecutorMetaStore struct{}

func (stubExecutorMetaStore) Get(string) (*orchestrator.ProjectMeta, bool) {
	return executorTestMeta(), true
}
func (stubExecutorMetaStore) GetWithWorkspace(context.Context, string) (*orchestrator.ProjectMeta, error) {
	return executorTestMeta(), nil
}

func executorTestMeta() *orchestrator.ProjectMeta {
	return &orchestrator.ProjectMeta{
		DefaultTaskBehavior: "dev",
		TaskBehaviors:       map[string]orchestrator.TaskBehavior{"dev": {}},
		BaseBranch:          "main",
	}
}
