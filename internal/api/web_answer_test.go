package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubAnswerService is a test double for PostAnswer handler.
type stubAnswerService struct {
	stubWebService
	answerErr   error
	answerCalls []answerCall
}

type answerCall struct {
	taskID     string
	questionID string
	answer     string
}

func (s *stubAnswerService) AnswerTask(ctx context.Context, taskID, questionID, answer string) error {
	s.answerCalls = append(s.answerCalls, answerCall{taskID: taskID, questionID: questionID, answer: answer})
	return s.answerErr
}

func newTestWebHandlerWithAnswer(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/answer", h.PostAnswer)
	return r
}

func TestWebHandler_PostAnswer_Success(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithAnswer(svc)

	form := url.Values{
		"question_id": {"qid-123"},
		"answer":      {"yes, proceed"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.answerCalls) != 1 {
		t.Fatalf("AnswerTask calls = %d, want 1", len(svc.answerCalls))
	}
	c := svc.answerCalls[0]
	if c.taskID != "task-1" {
		t.Errorf("taskID = %q, want task-1", c.taskID)
	}
	if c.questionID != "qid-123" {
		t.Errorf("questionID = %q, want qid-123", c.questionID)
	}
	if c.answer != "yes, proceed" {
		t.Errorf("answer = %q, want 'yes, proceed'", c.answer)
	}
}

func TestWebHandler_PostAnswer_HTMXRedirect(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithAnswer(svc)

	form := url.Values{
		"question_id": {"qid-abc"},
		"answer":      {"confirmed"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (HTMX)", w.Code)
	}
	redirect := w.Header().Get("HX-Redirect")
	if redirect != "/tasks/task-1" {
		t.Errorf("HX-Redirect = %q, want /tasks/task-1", redirect)
	}
}

func TestWebHandler_PostAnswer_ServiceError_RedirectsWithError(t *testing.T) {
	svc := &stubAnswerService{
		answerErr: fmt.Errorf("task is not awaiting"),
	}
	r := newTestWebHandlerWithAnswer(svc)

	form := url.Values{
		"question_id": {"qid-x"},
		"answer":      {"something"},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, should redirect to task page", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, should contain error param", loc)
	}
}

func TestWebHandler_PostAnswer_AnswerIsTrimmed(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithAnswer(svc)

	form := url.Values{
		"question_id": {"qid-1"},
		"answer":      {"  trimmed answer  "},
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/answer",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if len(svc.answerCalls) != 1 {
		t.Fatalf("AnswerTask calls = %d, want 1", len(svc.answerCalls))
	}
	if svc.answerCalls[0].answer != "trimmed answer" {
		t.Errorf("answer = %q, want trimmed", svc.answerCalls[0].answer)
	}
}

// TestWebHandler_TaskDetail_AwaitingShowsQASection verifies that the task
// detail page for an awaiting task includes the Q&A section markup.
func TestWebHandler_TaskDetail_AwaitingShowsQASection(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAwaiting
	// Embed a question in the payload as the awaiting trait.
	detail.Task.Payload = []byte(`{"awaiting":{"question":"What should we do?","question_id":"qid-1"}}`)

	svc := &stubAnswerService{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithAnswer(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "qa-section") {
		t.Errorf("awaiting task detail should contain qa-section, got HTML length %d", len(body))
	}
	if !strings.Contains(body, "What should we do?") {
		t.Errorf("qa-section should show the question text")
	}
	if !strings.Contains(body, `name="answer"`) {
		t.Errorf("qa-section should contain answer textarea")
	}
	if !strings.Contains(body, "回答を送信") {
		t.Errorf("qa-section should contain submit button")
	}
	if !strings.Contains(body, "拒否してタスクを中止") {
		t.Errorf("qa-section should contain abort button")
	}
}
