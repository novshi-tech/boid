package api

// LinkIdentity's action record, pinned against a real sqlite DB. The
// idempotent re-link must stay silent.

import (
	"context"
	"encoding/json"
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

	ctx := orchestrator.WithActor(context.Background(), orchestrator.ActorTask("t-writer"))
	if err := svc.LinkIdentity(ctx, "proj-1", "jira:ROOKPF-1", card.ID); err != nil {
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
	var got orchestrator.IdentityLinkedPayload
	if err := json.Unmarshal(actions[0].Payload, &got); err != nil {
		t.Fatalf("unmarshal payload %s: %v", actions[0].Payload, err)
	}
	if got.Identity != "jira:ROOKPF-1" {
		t.Errorf("payload identity = %q, want %q", got.Identity, "jira:ROOKPF-1")
	}
	if actions[0].Actor != orchestrator.ActorTask("t-writer") {
		t.Errorf("actor = %q, want the writing task", actions[0].Actor)
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

// TestUnlinkIdentity_Card_WritesIdentityUnlinkedAction: releasing a binding is
// the symmetric counterpart of making one, and changes the card the same way.
func TestUnlinkIdentity_Card_WritesIdentityUnlinkedAction(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	card := seedLinkCard(t, tasks)
	ctx := context.Background()
	if err := svc.LinkIdentity(ctx, "proj-1", "jira:ROOKPF-9", card.ID); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}

	if err := svc.UnlinkIdentity(ctx, "proj-1", "jira:ROOKPF-9"); err != nil {
		t.Fatalf("UnlinkIdentity: %v", err)
	}
	actions, err := tasks.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2 (linked then unlinked): %+v", len(actions), actions)
	}
	if actions[1].Type != orchestrator.ActionTypeIdentityUnlinked {
		t.Fatalf("second action = %q, want %q", actions[1].Type, orchestrator.ActionTypeIdentityUnlinked)
	}
	var got orchestrator.IdentityLinkedPayload
	if err := json.Unmarshal(actions[1].Payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got.Identity != "jira:ROOKPF-9" {
		t.Errorf("payload identity = %q, want %q", got.Identity, "jira:ROOKPF-9")
	}
}

// TestUnlinkIdentity_NoBinding_WritesNoAction: removing what was never there
// is not a change.
func TestUnlinkIdentity_NoBinding_WritesNoAction(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	card := seedLinkCard(t, tasks)

	if err := svc.UnlinkIdentity(context.Background(), "proj-1", "jira:NEVER-BOUND"); err != nil {
		t.Fatalf("UnlinkIdentity: %v", err)
	}
	actions, err := tasks.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("got %d actions, want 0: %+v", len(actions), actions)
	}
}

// TestLinkIdentity_NoTransactor_StillRecords pins the fallback branch: losing
// the transactor costs atomicity, never the record.
func TestLinkIdentity_NoTransactor_StillRecords(t *testing.T) {
	svc, tasks := newIdentityLinkTestService(t)
	svc.Tx = nil
	card := seedLinkCard(t, tasks)

	if err := svc.LinkIdentity(context.Background(), "proj-1", "jira:ROOKPF-7", card.ID); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	actions, err := tasks.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	if len(actions) != 1 || actions[0].Type != orchestrator.ActionTypeIdentityLinked {
		t.Fatalf("actions = %+v, want one %q", actions, orchestrator.ActionTypeIdentityLinked)
	}
}
