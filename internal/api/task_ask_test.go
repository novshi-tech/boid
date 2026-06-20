package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func blockingAwaitingPayload(t *testing.T, qid string) json.RawMessage {
	t.Helper()
	ap := orchestrator.AwaitingPayload{
		Question:   "Proceed?",
		QuestionID: qid,
		Mode:       orchestrator.AwaitingModeBlocking,
	}
	apJSON, err := json.Marshal(ap)
	if err != nil {
		t.Fatalf("marshal awaiting: %v", err)
	}
	payload, err := json.Marshal(map[string]json.RawMessage{string(orchestrator.TraitAwaiting): apJSON})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

// Blocking-mode answer must deliver the reply to the parked agent, flip the
// task back to executing, and NOT dispatch a resume (ApplyAction is never
// called) — the agent never exited.
func TestAnswerTask_BlockingMode_DeliversWithoutDispatch(t *testing.T) {
	reg := NewBlockingAskRegistry()
	if err := reg.Register("t1", "q-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	delivered := make(chan string, 1)
	go func() {
		ans, _ := reg.Wait(context.Background(), "q-1")
		delivered <- ans
	}()

	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   blockingAwaitingPayload(t, "q-1"),
	}
	wf := &stubWorkflowService{}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Actions:     actions,
		Workflow:    wf,
		BlockingAsk: reg,
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "the answer"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}

	select {
	case ans := <-delivered:
		if ans != "the answer" {
			t.Errorf("delivered answer = %q, want 'the answer'", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer was not delivered to the waiter")
	}

	if wf.appliedType != "" {
		t.Errorf("ApplyAction must NOT be called for a blocking answer (got %q); no resume dispatch", wf.appliedType)
	}
	if task.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("task status = %q, want executing after blocking answer", task.Status)
	}
	if actions.createdAction == nil || actions.createdAction.Type != "answer" {
		t.Errorf("expected an 'answer' audit action, got %+v", actions.createdAction)
	}
	// The awaiting trait must be stripped once consumed.
	if orchestrator.GetAwaitingPayload(task.Payload).QuestionID != "" {
		t.Errorf("awaiting trait should be stripped after blocking answer, payload=%s", task.Payload)
	}
}

// If no agent is parked (it disconnected), a blocking answer is refused rather
// than flipping the task to a zombie executing state.
func TestAnswerTask_BlockingMode_NoWaiterConflict(t *testing.T) {
	reg := NewBlockingAskRegistry() // nothing registered
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   blockingAwaitingPayload(t, "q-1"),
	}
	wf := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Workflow:    wf,
		BlockingAsk: reg,
	}

	err := svc.AnswerTask(context.Background(), "t1", "q-1", "answer")
	if err == nil {
		t.Fatal("expected a conflict when no agent is waiting")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
	if task.Status != orchestrator.TaskStatusAwaiting {
		t.Errorf("task status = %q, want unchanged awaiting when no waiter", task.Status)
	}
}

// Session-resume mode (the legacy notify --ask path) is unchanged: with no
// blocking mode in the awaiting trait, AnswerTask still drives ApplyAction.
func TestAnswerTask_SessionResumeMode_StillDispatches(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   json.RawMessage(`{"awaiting":{"question":"Q","question_id":"q-1","session_id":"sess-1"}}`),
	}
	wf := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Workflow: wf,
		// BlockingAsk intentionally nil: session_resume must not need it.
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}
	if wf.appliedType != "answer" {
		t.Errorf("applied action type = %q, want answer (session_resume path)", wf.appliedType)
	}
}

// AskTaskBlocking rejects a second concurrent ask for the same task with the B1
// error (a pre-registered question stands in for an in-flight ask).
func TestAskTaskBlocking_B1SecondAskFails(t *testing.T) {
	reg := NewBlockingAskRegistry()
	if err := reg.Register("t1", "q-existing"); err != nil {
		t.Fatalf("pre-Register: %v", err)
	}
	defer reg.Cancel("q-existing")

	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Payload:   json.RawMessage(`{}`),
	}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: reg,
	}

	_, err := svc.AskTaskBlocking(context.Background(), "t1", "Second?")
	if err == nil {
		t.Fatal("expected B1 error for a second concurrent ask")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// AskTaskBlocking refuses to ask from a non-executing task.
func TestAskTaskBlocking_NonExecutingRejected(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
	}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
	}
	_, err := svc.AskTaskBlocking(context.Background(), "t1", "Q?")
	if err == nil {
		t.Fatal("expected error asking from a non-executing task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}
