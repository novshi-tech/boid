package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubWebServiceWithRerun extends stubWebService with rerun/reopen support.
type stubWebServiceWithRerun struct {
	stubWebService
	rerunErr    error
	rerunCalls  []string
	reopenErr   error
	reopenCalls []ReopenTaskRequest
}

func (s *stubWebServiceWithRerun) RerunTask(id string, req RerunTaskRequest) error {
	s.rerunCalls = append(s.rerunCalls, id)
	return s.rerunErr
}

func (s *stubWebServiceWithRerun) ReopenTask(id string, req ReopenTaskRequest) error {
	s.reopenCalls = append(s.reopenCalls, req)
	return s.reopenErr
}

func (s *stubWebServiceWithRerun) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	return nil, nil
}

func (s *stubWebServiceWithRerun) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	return &ReplayHookResult{}, nil
}

func newTestWebHandlerWithRerun(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	return r
}

func TestWebHandler_PostRerun_Success(t *testing.T) {
	svc := &stubWebServiceWithRerun{}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/rerun", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.rerunCalls) != 1 || svc.rerunCalls[0] != "task-1" {
		t.Errorf("RerunTask not called correctly: %v", svc.rerunCalls)
	}
}

func TestWebHandler_PostRerun_Error(t *testing.T) {
	svc := &stubWebServiceWithRerun{rerunErr: fmt.Errorf("rerun failed")}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/rerun", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to task", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebHandler_TaskDetail_ContainsRerunButton(t *testing.T) {
	// Rerun is the primary action only in terminal states (done/aborted);
	// executing tasks show Abort instead. Override the default stub status
	// so the action bar renders the rerun form.
	detail := makeTaskDetailView()
	detail.Task.Status = orchestrator.TaskStatusDone
	svc := &stubWebServiceWithRerun{
		stubWebService: stubWebService{taskDetail: detail},
	}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Rerun this task?") {
		t.Errorf("task detail should contain rerun confirm text, got: %s", body)
	}
	if !strings.Contains(body, "rerun") {
		t.Errorf("task detail should contain rerun button, got: %s", body)
	}
}
