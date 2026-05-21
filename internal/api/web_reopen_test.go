package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newTestWebHandlerWithReopen(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/reopen", h.ReopenForm)
	r.Post("/tasks/{id}/reopen", h.PostReopen)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	return r
}

func TestWebHandler_ReopenForm_Success(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusDone
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/reopen", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Reopen") {
		t.Errorf("form should contain Reopen button, got: %s", body)
	}
	if !strings.Contains(body, "<textarea") {
		t.Errorf("form should contain textarea, got: %s", body)
	}
	if !strings.Contains(body, `name="message"`) {
		t.Errorf("form should contain message textarea, got: %s", body)
	}
}

func TestWebHandler_ReopenForm_NotDoneRedirects(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusExecuting
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/reopen", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to task", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebHandler_PostReopen_WithMessage(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusDone
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	form := url.Values{"message": {"fix PR review"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/reopen",
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
	if len(svc.reopenCalls) != 1 {
		t.Fatalf("ReopenTask calls = %d, want 1", len(svc.reopenCalls))
	}
	if svc.reopenCalls[0].Message != "fix PR review" {
		t.Errorf("ReopenTask message = %q, want 'fix PR review'", svc.reopenCalls[0].Message)
	}
}

func TestWebHandler_PostReopen_EmptyMessage(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusDone
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	form := url.Values{"message": {""}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/reopen",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if len(svc.reopenCalls) != 1 {
		t.Fatalf("ReopenTask calls = %d, want 1", len(svc.reopenCalls))
	}
	if svc.reopenCalls[0].Message != "" {
		t.Errorf("ReopenTask message = %q, want empty", svc.reopenCalls[0].Message)
	}
}

func TestWebHandler_PostReopen_Error(t *testing.T) {
	svc := &stubWebServiceWithRerun{reopenErr: fmt.Errorf("state conflict")}
	r := newTestWebHandlerWithReopen(svc)

	form := url.Values{"message": {"msg"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/reopen",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to task", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebHandler_TaskDetail_DoneContainsReopenAndRerunButtons(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusDone
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/tasks/task-1/reopen") {
		t.Errorf("done task detail should contain reopen link, got: %s", body)
	}
	if !strings.Contains(body, "Reopen") {
		t.Errorf("done task detail should contain Reopen button text, got: %s", body)
	}
	if !strings.Contains(body, "rerun") {
		t.Errorf("done task detail should contain rerun button, got: %s", body)
	}
}

func TestWebHandler_TaskDetail_AbortedContainsOnlyRerun(t *testing.T) {
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusAborted
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithReopen(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "/tasks/task-1/reopen") {
		t.Errorf("aborted task detail should NOT contain reopen link, got: %s", body)
	}
	if !strings.Contains(body, "rerun") {
		t.Errorf("aborted task detail should contain rerun button, got: %s", body)
	}
}
