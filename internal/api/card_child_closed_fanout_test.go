package api

// Pins that recordChildClosedOnParent and recordVanishedChildClosedOnParent
// broadcast the child_closed action they already write onto the parent's
// own action log. Uses a real sqlite DB (newCardCommandTestService) since
// both write real task_triage rows through the real orchestrator.CreateAction
// pipeline.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestRecordChildClosedOnParent_BroadcastsToParentCard(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(nil))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)

	child := &orchestrator.Task{
		Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID,
		Title: "fix the thing", Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}
	if err := repo.CreateTask(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: child.ID,
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	hub := NewTaskEventHub()
	svc.Hub = hub
	parentCh := hub.Subscribe(context.Background(), card.ID)

	child.Status = orchestrator.TaskStatusDone
	svc.recordChildClosedOnParent(context.Background(), child)

	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the child_closed broadcast")
	}
	if ev.Kind != "action" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "action")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok || payload["action_id"] == "" || payload["action_id"] == nil {
		t.Fatalf("payload = %v, want a non-empty action_id", ev.Payload)
	}

	actions, err := repo.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	found := false
	for _, a := range actions {
		if a.Type == "child_closed" {
			found = true
			if payload["action_id"] != a.ID {
				t.Errorf("broadcast action_id = %v, want the actually-persisted action id %q", payload["action_id"], a.ID)
			}
		}
	}
	if !found {
		t.Fatal("expected a child_closed action on the parent's own action log")
	}
}

// TestRecordChildClosedOnParent_NoBroadcastOnCommitFailure pins that the
// broadcast sits AFTER WithinTx returns, not inside its closure: wrapping
// the same real repo in postCommitFailTransactor still runs the closure
// body (the task_triage/action writes actually land), but WithinTx itself
// reports failure — a mutation moving the broadcast inside the closure
// would still fire it.
func TestRecordChildClosedOnParent_NoBroadcastOnCommitFailure(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(nil))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)

	child := &orchestrator.Task{
		Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID,
		Title: "fix the thing", Status: orchestrator.TaskStatusExecuting,
		Exec: &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}
	if err := repo.CreateTask(child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: child.ID,
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	hub := NewTaskEventHub()
	svc.Hub = hub
	svc.Tx = postCommitFailTransactor{inner: realTaskRepoTxStore{repo}}
	parentCh := hub.Subscribe(context.Background(), card.ID)

	child.Status = orchestrator.TaskStatusDone
	svc.recordChildClosedOnParent(context.Background(), child)

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive a broadcast when WithinTx reports failure")
	}
}

func TestRecordChildClosedOnParent_NoBroadcastWhenParentIsNotACard(t *testing.T) {
	svc, _, _ := newCardCommandTestService(t, "proj-1", testCardMeta(nil))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)

	// An ordinary supervisor/executor pair: the parent is itself an
	// execution task, so recordChildClosedOnParent's early
	// GetTaskTriage(sql.ErrNoRows) return keeps it a no-op — no
	// child_closed action, and nothing to broadcast either.
	parent := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "supervisor"}}
	if err := repo.CreateTask(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: parent.ID, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{Behavior: "executor"}}
	if err := repo.CreateTask(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	hub := NewTaskEventHub()
	svc.Hub = hub
	parentCh := hub.Subscribe(context.Background(), parent.ID)

	svc.recordChildClosedOnParent(context.Background(), child)

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("execution parent must not receive a child_closed broadcast")
	}
}

func TestRecordVanishedChildClosedOnParent_BroadcastsToParentCard(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(nil))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)

	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "vanished-task-id",
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	hub := NewTaskEventHub()
	svc.Hub = hub
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc.recordVanishedChildClosedOnParent(context.Background(), card.ID, "vanished-task-id")

	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the vanished child_closed broadcast")
	}
	if ev.Kind != "action" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "action")
	}

	actions, err := repo.ListActionsByTask(card.ID)
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	found := false
	for _, a := range actions {
		if a.Type != "child_closed" {
			continue
		}
		var p struct {
			ChildID     string `json:"child_id"`
			ChildStatus string `json:"child_status"`
		}
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			t.Fatalf("unmarshal action payload: %v", err)
		}
		if p.ChildID == "vanished-task-id" && p.ChildStatus == "vanished" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a vanished child_closed action on the parent's own action log")
	}
}

// TestRecordVanishedChildClosedOnParent_NoBroadcastOnCommitFailure is
// TestRecordChildClosedOnParent_NoBroadcastOnCommitFailure's vanished-child
// counterpart.
func TestRecordVanishedChildClosedOnParent_NoBroadcastOnCommitFailure(t *testing.T) {
	svc, _, card := newCardCommandTestService(t, "proj-1", testCardMeta(nil))
	repo := svc.CardRequests.(*orchestrator.TaskRepository)

	detail, err := orchestrator.AddDetailChild(nil, orchestrator.TaskTriageChild{
		ID: "c1", Status: orchestrator.TaskTriageChildStatusDispatched, TaskRef: "vanished-task-id",
	})
	if err != nil {
		t.Fatalf("AddDetailChild: %v", err)
	}
	if err := repo.UpsertTaskTriage(&orchestrator.CardAttrs{TaskID: card.ID, Detail: detail}); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	hub := NewTaskEventHub()
	svc.Hub = hub
	svc.Tx = postCommitFailTransactor{inner: realTaskRepoTxStore{repo}}
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc.recordVanishedChildClosedOnParent(context.Background(), card.ID, "vanished-task-id")

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive a broadcast when WithinTx reports failure")
	}
}
