package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestAwaitingPayload_RoundTrip(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	payload := json.RawMessage(`{"awaiting":{"session_id":"sess-abc","question":"Should I proceed?","question_id":"qid-1","pending_answer":"yes"}}`)
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "Awaiting Task",
		Behavior:  "dev",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   payload,
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}

	if got.Status != orchestrator.TaskStatusAwaiting {
		t.Errorf("status: got %q, want %q", got.Status, orchestrator.TaskStatusAwaiting)
	}

	ap := orchestrator.GetAwaitingPayload(got.Payload)
	if ap.SessionID != "sess-abc" {
		t.Errorf("session_id: got %q, want %q", ap.SessionID, "sess-abc")
	}
	if ap.Question != "Should I proceed?" {
		t.Errorf("question: got %q, want %q", ap.Question, "Should I proceed?")
	}
	if ap.QuestionID != "qid-1" {
		t.Errorf("question_id: got %q, want %q", ap.QuestionID, "qid-1")
	}
	if ap.PendingAnswer != "yes" {
		t.Errorf("pending_answer: got %q, want %q", ap.PendingAnswer, "yes")
	}
}

func TestGetAwaitingPayload_AbsentTrait(t *testing.T) {
	ap := orchestrator.GetAwaitingPayload(json.RawMessage(`{"artifact":{"url":"x"}}`))
	if ap != (orchestrator.AwaitingPayload{}) {
		t.Errorf("expected empty AwaitingPayload, got %+v", ap)
	}
}

func TestGetAwaitingPayload_EmptyPayload(t *testing.T) {
	ap := orchestrator.GetAwaitingPayload(json.RawMessage(`{}`))
	if ap != (orchestrator.AwaitingPayload{}) {
		t.Errorf("expected empty AwaitingPayload, got %+v", ap)
	}
}
