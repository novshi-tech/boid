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

// If no agent is parked (its `boid task ask` was killed by a harness
// command-timeout), the answer is persisted durably to PendingAnswer and the
// task stays awaiting. The agent picks the answer up on its next ask. This is
// the disconnect-resilience contract: an answer that arrives between disconnect
// and re-ask is never dropped, and the task is not flipped to a zombie
// executing state with no live agent behind it.
func TestAnswerTask_NoWaiter_PersistsDurably(t *testing.T) {
	reg := NewBlockingAskRegistry() // nothing registered
	store := &stubTaskStore{task: &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   blockingAwaitingPayload(t, "q-1"),
	}}
	wf := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:       store,
		Workflow:    wf,
		BlockingAsk: reg,
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "the answer"); err != nil {
		t.Fatalf("AnswerTask should persist durably when no waiter, got %v", err)
	}
	if store.task.Status != orchestrator.TaskStatusAwaiting {
		t.Errorf("task status = %q, want awaiting (answer parked, no live agent)", store.task.Status)
	}
	if got := orchestrator.GetAwaitingPayload(store.task.Payload).PendingAnswer; got != "the answer" {
		t.Errorf("pending_answer = %q, want %q (durable parking)", got, "the answer")
	}
	if wf.appliedType != "" {
		t.Errorf("ApplyAction must NOT be called for a parked answer, got %q", wf.appliedType)
	}
}

// Legacy `notify --ask` awaiting records (carrying a session_id field the
// deserialiser ignores) reach AnswerTask with no parked agent. Same as any
// other no-waiter case: the answer is parked durably rather than rejected. The
// awaiting reaper (PR2) eventually retires it if no agent ever returns.
func TestAnswerTask_LegacyAwaitingWithoutWaiter_PersistsDurably(t *testing.T) {
	store := &stubTaskStore{task: &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   json.RawMessage(`{"awaiting":{"question":"Q","question_id":"q-1","session_id":"sess-1"}}`),
	}}
	svc := &TaskAppService{
		Tasks:       store,
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes"); err != nil {
		t.Fatalf("AnswerTask should persist durably, got %v", err)
	}
	if got := orchestrator.GetAwaitingPayload(store.task.Payload).PendingAnswer; got != "yes" {
		t.Errorf("pending_answer = %q, want yes", got)
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

// An awaiting task with no pending question id (e.g. a pending/done task, or a
// malformed awaiting payload) cannot be asked/re-asked: there is no episode to
// attach to. Reject with a conflict rather than block forever.
func TestAskTaskBlocking_AwaitingWithoutQuestionRejected(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting, // no awaiting payload → no qid
	}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
	}
	_, err := svc.AskTaskBlocking(context.Background(), "t1", "Q?")
	if err == nil {
		t.Fatal("expected error asking an awaiting task with no pending question")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// A task in a terminal/non-askable status (e.g. done) is rejected.
func TestAskTaskBlocking_NonAskableStatusRejected(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusDone,
	}
	svc := &TaskAppService{
		Tasks:       &stubTaskStore{task: task},
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
	}
	_, err := svc.AskTaskBlocking(context.Background(), "t1", "Q?")
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected 409 StatusError, got %v", err)
	}
}

// Re-asking an awaiting task whose answer already arrived (parked in
// PendingAnswer during a disconnect) returns the answer immediately, flips the
// task to executing, and strips the awaiting trait — no blocking. This is the
// recovery half of disconnect resilience: the killed-and-retried `boid task
// ask` picks up the durable answer on its next attempt.
func TestAskTaskBlocking_ReAsk_ConsumesPendingAnswer(t *testing.T) {
	payload := json.RawMessage(`{"awaiting":{"question":"Proceed?","question_id":"q-1","pending_answer":"yes go"}}`)
	store := &stubTaskStore{task: &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   payload,
	}}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:       store,
		Actions:     actions,
		Workflow:    &stubWorkflowService{},
		BlockingAsk: NewBlockingAskRegistry(),
	}

	ans, err := svc.AskTaskBlocking(context.Background(), "t1", "Proceed?")
	if err != nil {
		t.Fatalf("re-ask should consume parked answer, got %v", err)
	}
	if ans != "yes go" {
		t.Errorf("answer = %q, want %q", ans, "yes go")
	}
	if store.task.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("task status = %q, want executing after consuming answer", store.task.Status)
	}
	if orchestrator.GetAwaitingPayload(store.task.Payload).QuestionID != "" {
		t.Errorf("awaiting trait should be stripped after consume, payload=%s", store.task.Payload)
	}
	if actions.createdAction == nil || actions.createdAction.Type != "answer" {
		t.Errorf("expected an 'answer' audit action, got %+v", actions.createdAction)
	}
}

// Re-asking an awaiting task with no parked answer re-attaches to the existing
// question id and blocks again. A subsequent answer is delivered over the
// re-attached channel. This proves the kill→retry loop converges without a
// fresh ask transition.
func TestAskTaskBlocking_ReAsk_ReattachesAndBlocks(t *testing.T) {
	store := &stubTaskStore{task: &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Payload:   blockingAwaitingPayload(t, "q-1"),
	}}
	reg := NewBlockingAskRegistry()
	wf := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:       store,
		Actions:     &capturingActionStore{},
		Workflow:    wf,
		BlockingAsk: reg,
	}

	type result struct {
		ans string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := svc.AskTaskBlocking(context.Background(), "t1", "Proceed?")
		done <- result{ans, err}
	}()

	// The re-ask must re-register the existing qid (no fresh "ask" transition).
	if !waitUntil(func() bool { return reg.Has("q-1") }) {
		t.Fatal("re-ask never re-registered the existing question")
	}
	if wf.appliedType != "" {
		t.Errorf("re-ask must NOT fire a fresh ask transition, got %q", wf.appliedType)
	}

	// Deliver the answer via the normal answer path (fast-path Notify + flip).
	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "late answer"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("re-attached ask returned error: %v", r.err)
		}
		if r.ans != "late answer" {
			t.Errorf("answer = %q, want %q", r.ans, "late answer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-attached ask did not return after answer")
	}
}
