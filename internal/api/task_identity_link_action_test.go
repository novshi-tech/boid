package api

// LinkIdentity's action record, pinned against a real sqlite DB. The
// idempotent re-link must stay silent.

import (
	"context"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newIdentityLinkTestService(t *testing.T) (*TaskAppService, *orchestrator.TaskRepository) {
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
	tasks := orchestrator.NewTaskRepository(d.Conn)
	return &TaskAppService{
		Tasks:      tasks,
		Actions:    tasks,
		Identities: tasks,
		Tx:         realTransactor{conn: d.Conn},
	}, tasks
}

func seedLinkCard(t *testing.T, tasks *orchestrator.TaskRepository) *orchestrator.Task {
	t.Helper()
	card := &orchestrator.Task{
		ProjectID: "proj-1", Type: orchestrator.TaskTypeCard,
		Title: "a card", Status: orchestrator.TaskStatusParked,
		Card: &orchestrator.CardAttrs{},
	}
	if err := tasks.CreateTask(card); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return card
}

func TestLinkIdentity_NewBinding_WritesIdentityLinkedAction(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	card := seedLinkCard(t, tasks)

	if err := svc.LinkIdentity(context.Background(), "proj-1", "jira:ROOKPF-1", card.ID); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	actions, err := tasks.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1: %+v", len(actions), actions)
	}
	if actions[0].Type != orchestrator.ActionTypeIdentityLinked {
		t.Fatalf("action type = %q, want %q", actions[0].Type, orchestrator.ActionTypeIdentityLinked)
	}
	if !strings.Contains(string(actions[0].Payload), "jira:ROOKPF-1") {
		t.Errorf("payload does not carry the identity: %s", actions[0].Payload)
	}
}

func TestLinkIdentity_SameBindingAgain_WritesNoAction(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	card := seedLinkCard(t, tasks)

	for i := 0; i < 2; i++ {
		if err := svc.LinkIdentity(context.Background(), "proj-1", "jira:ROOKPF-1", card.ID); err != nil {
			t.Fatalf("LinkIdentity #%d: %v", i+1, err)
		}
	}
	actions, err := tasks.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("re-linking the same identity wrote %d actions, want 1 total: %+v", len(actions), actions)
	}
}

func TestLinkIdentity_ExecutionTask_WritesNoAction(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	task := &orchestrator.Task{
		ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution,
		Title: "t", Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := tasks.CreateTask(task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := svc.LinkIdentity(context.Background(), "proj-1", "jira:ROOKPF-2", task.ID); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	actions, err := tasks.ListActionsByTask(task.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("execution task wrote %d actions, want 0: %+v", len(actions), actions)
	}
}
