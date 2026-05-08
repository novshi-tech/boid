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

func TestClearPendingAnswer_RemovesAnswerKeepsRest(t *testing.T) {
	payload := json.RawMessage(`{"awaiting":{"session_id":"sess-1","question":"q","question_id":"qid-1","pending_answer":"yes"}}`)
	got := orchestrator.ClearPendingAnswer(payload)

	ap := orchestrator.GetAwaitingPayload(got)
	if ap.PendingAnswer != "" {
		t.Errorf("pending_answer = %q, want empty", ap.PendingAnswer)
	}
	if ap.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", ap.SessionID)
	}
	if ap.QuestionID != "qid-1" {
		t.Errorf("question_id = %q, want qid-1", ap.QuestionID)
	}
	if ap.Question != "q" {
		t.Errorf("question = %q, want q", ap.Question)
	}
}

func TestClearPendingAnswer_NoOpWhenAbsent(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"artifact":{"url":"x"}}`),
		json.RawMessage(`{"awaiting":{"session_id":"s","question_id":"q"}}`),
	}
	for _, payload := range cases {
		got := orchestrator.ClearPendingAnswer(payload)
		if string(got) != string(payload) {
			t.Errorf("ClearPendingAnswer changed payload unexpectedly: %s → %s", payload, got)
		}
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

func TestStripAwaitingTrait_RemovesTraitKeepsRest(t *testing.T) {
	in := json.RawMessage(`{"awaiting":{"question":"x","question_id":"q1"},"artifact":{"url":"y"}}`)
	out := orchestrator.StripAwaitingTrait(in)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := got["awaiting"]; ok {
		t.Errorf("awaiting trait should be removed, got %s", out)
	}
	if _, ok := got["artifact"]; !ok {
		t.Errorf("artifact trait should be preserved, got %s", out)
	}
}

func TestStripAwaitingTrait_NoOpWhenAbsent(t *testing.T) {
	in := json.RawMessage(`{"artifact":{"url":"y"}}`)
	out := orchestrator.StripAwaitingTrait(in)
	if string(out) != string(in) {
		t.Errorf("expected payload unchanged, got %s", out)
	}
}

func TestStripAwaitingTrait_EmptyPayload(t *testing.T) {
	if got := orchestrator.StripAwaitingTrait(nil); got != nil {
		t.Errorf("expected nil, got %s", got)
	}
	if got := orchestrator.StripAwaitingTrait(json.RawMessage(``)); len(got) != 0 {
		t.Errorf("expected empty, got %s", got)
	}
}
