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

// stubWebServiceWithHooks extends stubWebService with hook replay support.
type stubWebServiceWithHooks struct {
	stubWebService
	hooks           []orchestrator.Hook
	hooksErr        error
	replayHookErr   error
	replayHookCalls []ReplayHookRequest
}

func (s *stubWebServiceWithHooks) ListGatesForStatus(taskID, status string) ([]orchestrator.Gate, error) {
	return nil, nil
}

func (s *stubWebServiceWithHooks) ReplayGate(ctx context.Context, taskID string, req ReplayGateRequest) (*ReplayGateResult, error) {
	return &ReplayGateResult{Task: &orchestrator.Task{ID: taskID}}, nil
}

func (s *stubWebServiceWithHooks) ListHooksForStatus(taskID, status string) ([]orchestrator.Hook, error) {
	return s.hooks, s.hooksErr
}

func (s *stubWebServiceWithHooks) ReplayHook(ctx context.Context, taskID string, req ReplayHookRequest) (*ReplayHookResult, error) {
	s.replayHookCalls = append(s.replayHookCalls, req)
	if s.replayHookErr != nil {
		return nil, s.replayHookErr
	}
	return &ReplayHookResult{Task: &orchestrator.Task{ID: taskID}}, nil
}

func (s *stubWebServiceWithHooks) RerunTask(id string, req RerunTaskRequest) error { return nil }
func (s *stubWebServiceWithHooks) ReopenTask(id string, req ReopenTaskRequest) error {
	return nil
}

func newTestWebHandlerWithHooks(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}/hooks", h.HookReplayList)
	r.Post("/tasks/{id}/hooks/{hook_id}/replay", h.PostHookReplay)
	return r
}

func TestWebHandler_HookReplayList_Renders(t *testing.T) {
	svc := &stubWebServiceWithHooks{
		hooks: []orchestrator.Hook{
			{ID: "run-agent", Kit: "claude-code"},
			{ID: "notify"},
		},
	}
	r := newTestWebHandlerWithHooks(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/hooks", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "run-agent") {
		t.Errorf("response should contain hook ID 'run-agent', got: %s", body)
	}
	if !strings.Contains(body, "notify") {
		t.Errorf("response should contain hook ID 'notify', got: %s", body)
	}
	if !strings.Contains(body, "replay") {
		t.Errorf("response should contain replay button, got: %s", body)
	}
	if !strings.Contains(body, "本当にフックを再発火しますか") {
		t.Errorf("response should contain confirm dialog text, got: %s", body)
	}
}

func TestWebHandler_HookReplayList_Empty(t *testing.T) {
	svc := &stubWebServiceWithHooks{hooks: []orchestrator.Hook{}}
	r := newTestWebHandlerWithHooks(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/hooks", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "再発火可能なフックがありません") {
		t.Errorf("response should indicate no hooks, got: %s", body)
	}
}

func TestWebHandler_PostHookReplay_Success(t *testing.T) {
	svc := &stubWebServiceWithHooks{}
	r := newTestWebHandlerWithHooks(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/hooks/run-agent/replay", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.replayHookCalls) != 1 {
		t.Fatalf("ReplayHook calls = %d, want 1", len(svc.replayHookCalls))
	}
	if svc.replayHookCalls[0].HookID != "run-agent" {
		t.Errorf("ReplayHook HookID = %q, want run-agent", svc.replayHookCalls[0].HookID)
	}
}

func TestWebHandler_PostHookReplay_Error(t *testing.T) {
	svc := &stubWebServiceWithHooks{replayHookErr: fmt.Errorf("hook not found")}
	r := newTestWebHandlerWithHooks(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/hooks/missing/replay", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1/hooks") {
		t.Errorf("Location = %q, want redirect to hooks page", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}
