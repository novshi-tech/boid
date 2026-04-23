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

// stubWebServiceWithGates extends stubWebService with gate replay support.
type stubWebServiceWithGates struct {
	stubWebService
	rerunErr       error
	rerunCalls     []string
	gates          []orchestrator.Gate
	gatesErr       error
	replayGateErr  error
	replayGateCalls []ReplayGateRequest
}

func (s *stubWebServiceWithGates) RerunTask(id string, req RerunTaskRequest) error {
	s.rerunCalls = append(s.rerunCalls, id)
	return s.rerunErr
}

func (s *stubWebServiceWithGates) ListGatesForStatus(taskID, status string) ([]orchestrator.Gate, error) {
	return s.gates, s.gatesErr
}

func (s *stubWebServiceWithGates) ReplayGate(ctx context.Context, taskID string, req ReplayGateRequest) (*ReplayGateResult, error) {
	s.replayGateCalls = append(s.replayGateCalls, req)
	if s.replayGateErr != nil {
		return nil, s.replayGateErr
	}
	return &ReplayGateResult{Task: &orchestrator.Task{ID: taskID}}, nil
}

func newTestWebHandlerWithRerun(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Get("/tasks/{id}/gates", h.GateReplayList)
	r.Post("/tasks/{id}/gates/{gate_id}/replay", h.PostGateReplay)
	return r
}

func TestWebHandler_PostRerun_Success(t *testing.T) {
	svc := &stubWebServiceWithGates{}
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
	svc := &stubWebServiceWithGates{rerunErr: fmt.Errorf("rerun failed")}
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

func TestWebHandler_GateReplayList_Renders(t *testing.T) {
	svc := &stubWebServiceWithGates{
		gates: []orchestrator.Gate{
			{ID: "pr-verify", Phase: orchestrator.GatePhaseExit, Kit: "go-dev"},
			{ID: "auto-merge", Phase: orchestrator.GatePhaseEntry},
		},
	}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/gates", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "pr-verify") {
		t.Errorf("response should contain gate ID 'pr-verify', got: %s", body)
	}
	if !strings.Contains(body, "auto-merge") {
		t.Errorf("response should contain gate ID 'auto-merge', got: %s", body)
	}
	if !strings.Contains(body, "replay") {
		t.Errorf("response should contain replay button, got: %s", body)
	}
	if !strings.Contains(body, "本当にゲートを再発火しますか") {
		t.Errorf("response should contain confirm dialog text, got: %s", body)
	}
}

func TestWebHandler_GateReplayList_Empty(t *testing.T) {
	svc := &stubWebServiceWithGates{gates: []orchestrator.Gate{}}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1/gates", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "再発火可能なゲートがありません") {
		t.Errorf("response should indicate no gates, got: %s", body)
	}
}

func TestWebHandler_PostGateReplay_Success(t *testing.T) {
	svc := &stubWebServiceWithGates{}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/gates/pr-verify/replay", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.replayGateCalls) != 1 {
		t.Fatalf("ReplayGate calls = %d, want 1", len(svc.replayGateCalls))
	}
	if svc.replayGateCalls[0].GateID != "pr-verify" {
		t.Errorf("ReplayGate GateID = %q, want pr-verify", svc.replayGateCalls[0].GateID)
	}
}

func TestWebHandler_PostGateReplay_Error(t *testing.T) {
	svc := &stubWebServiceWithGates{replayGateErr: fmt.Errorf("gate not found")}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/gates/missing/replay", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1/gates") {
		t.Errorf("Location = %q, want redirect to gates page", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebHandler_TaskDetail_ContainsRerunButton(t *testing.T) {
	svc := &stubWebServiceWithGates{
		stubWebService: stubWebService{taskDetail: makeTaskDetailView()},
	}
	r := newTestWebHandlerWithRerun(svc)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, url.QueryEscape("本当に再実行しますか")) && !strings.Contains(body, "本当に再実行しますか") {
		t.Errorf("task detail should contain rerun confirm text, got: %s", body)
	}
	if !strings.Contains(body, "rerun") {
		t.Errorf("task detail should contain rerun button, got: %s", body)
	}
	// gate replay is no longer on task detail; it's now accessible from JobDetail
	if strings.Contains(body, "/tasks/task-1/gates") {
		t.Errorf("task detail should not contain a link to /tasks/{id}/gates (moved to JobDetail)")
	}
}
