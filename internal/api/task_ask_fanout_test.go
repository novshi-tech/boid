package api

// Pins that the awaiting→executing answer path broadcasts — the direction
// task_ask.go's own gap grep couldn't find, since a call site that never
// calls Broadcast at all doesn't show up when auditing existing broadcast
// call sites. recordAnswerAction now routes through broadcastNotifyAction
// the same way NotifyTask's progress/done_request/fail_request do.

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestAnswerTask_BlockingMode_BroadcastsSelfAndFansOutToParentCard covers
// answerBlocking's fast path (a live waiter): the answer flips the child
// awaiting→executing, and that flip must reach both the child's own
// subscribers and its parent card.
func TestAnswerTask_BlockingMode_BroadcastsSelfAndFansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{
		ID: "child-1", ProjectID: "proj-1", ParentID: card.ID,
		Status: orchestrator.TaskStatusAwaiting, Type: orchestrator.TaskTypeExecution,
		Exec: &orchestrator.ExecAttrs{Payload: blockingAwaitingPayload(t, "q-1")},
	}

	reg := NewBlockingAskRegistry()
	if err := reg.Register(child.ID, "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	delivered := make(chan string, 1)
	go func() {
		ans, _ := reg.Wait(context.Background(), "q-1")
		delivered <- ans
	}()

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskAppService{
		Tasks:       &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Actions:     &capturingActionStore{},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: reg,
		Hub:         hub,
	}

	if err := svc.AnswerTask(context.Background(), child.ID, "q-1", "go ahead"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("answer was not delivered to the waiter")
	}
	if child.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want executing", child.Status)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("the answered child's own subscriber did not receive a broadcast")
	}
	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the answer fan-out event")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}

// TestAskTaskBlocking_ReAsk_ConsumePendingAnswer_BroadcastsSelfAndFansOutToParentCard
// covers consumePendingAnswer (the re-ask recovery path — an answer parked
// while the agent was disconnected, delivered on its next ask).
func TestAskTaskBlocking_ReAsk_ConsumePendingAnswer_BroadcastsSelfAndFansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{
		ID: "child-1", ProjectID: "proj-1", ParentID: card.ID,
		Status: orchestrator.TaskStatusAwaiting, Type: orchestrator.TaskTypeExecution,
		Exec: &orchestrator.ExecAttrs{Payload: []byte(`{"awaiting":{"question":"Proceed?","question_id":"q-1","pending_answer":"yes go"}}`)},
	}

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskAppService{
		Tasks:       &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Actions:     &capturingActionStore{},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
		Hub:         hub,
	}

	ans, err := svc.AskTaskBlocking(context.Background(), child.ID, "Proceed?")
	if err != nil {
		t.Fatalf("AskTaskBlocking (re-ask): %v", err)
	}
	if ans != "yes go" {
		t.Fatalf("answer = %q, want %q", ans, "yes go")
	}
	if child.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want executing", child.Status)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("the answered child's own subscriber did not receive a broadcast")
	}
	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the answer fan-out event")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}
