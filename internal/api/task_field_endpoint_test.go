package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func withChiURLParam(req *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

type fieldTaskStore struct {
	tasks   map[string]*orchestrator.Task
	actions map[string][]*orchestrator.Action
}

func (s *fieldTaskStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *fieldTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	if t, ok := s.tasks[id]; ok {
		return t, nil
	}
	return nil, &StatusError{Code: http.StatusNotFound, Message: "task not found: " + id}
}
func (s *fieldTaskStore) ListTasks(_ orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *fieldTaskStore) UpdateTask(_ *orchestrator.Task) error { return nil }
func (s *fieldTaskStore) DeleteTask(_ string) error             { return nil }
func (s *fieldTaskStore) FindTaskByRemote(_, _ string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *fieldTaskStore) FindTaskByRef(_, _ string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *fieldTaskStore) FindDependentTasks(_ string) ([]*orchestrator.Task, error) {
	return nil, nil
}

type fieldActionStore struct {
	actions map[string][]*orchestrator.Action
}

func (s *fieldActionStore) CreateAction(_ *orchestrator.Action) error { return nil }
func (s *fieldActionStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return s.actions[taskID], nil
}

func TestTaskHandler_Field_TopLevel(t *testing.T) {
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Title: "hello", Status: orchestrator.TaskStatusExecuting},
		},
	}
	svc := &TaskAppService{Tasks: store, Actions: &fieldActionStore{}}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?path=status", nil)
	req = withChiURLParam(req, "id", "t1")
	rr := httptest.NewRecorder()
	h.Field(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "executing" {
		t.Errorf("body = %q, want %q", got, "executing")
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestTaskHandler_Field_PayloadAutoFallback(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"awaiting": map[string]string{
			"question": "are you sure?",
		},
	})
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Payload: payload},
		},
	}
	svc := &TaskAppService{Tasks: store, Actions: &fieldActionStore{}}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?path=awaiting.question", nil)
	req = withChiURLParam(req, "id", "t1")
	rr := httptest.NewRecorder()
	h.Field(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "are you sure?" {
		t.Errorf("body = %q, want %q", got, "are you sure?")
	}
}

func TestTaskHandler_Field_LifecycleDerivedFromActions(t *testing.T) {
	abortPayload, _ := json.Marshal(map[string]string{
		"code":    "user_aborted",
		"message": "stopped",
	})
	store := &fieldTaskStore{
		tasks: map[string]*orchestrator.Task{
			"t1": {ID: "t1", Status: orchestrator.TaskStatusAborted},
		},
	}
	actions := &fieldActionStore{
		actions: map[string][]*orchestrator.Action{
			"t1": {{
				TaskID:   "t1",
				ToStatus: orchestrator.TaskStatusAborted,
				Payload:  abortPayload,
			}},
		},
	}
	svc := &TaskAppService{Tasks: store, Actions: actions}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?path=lifecycle.abort.message", nil)
	req = withChiURLParam(req, "id", "t1")
	rr := httptest.NewRecorder()
	h.Field(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%q", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "stopped" {
		t.Errorf("body = %q, want %q", got, "stopped")
	}
}

func TestTaskHandler_Field_MissingPath(t *testing.T) {
	store := &fieldTaskStore{tasks: map[string]*orchestrator.Task{"t1": {ID: "t1"}}}
	svc := &TaskAppService{Tasks: store, Actions: &fieldActionStore{}}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = withChiURLParam(req, "id", "t1")
	rr := httptest.NewRecorder()
	h.Field(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestTaskHandler_Field_UnknownTask(t *testing.T) {
	svc := &TaskAppService{Tasks: &fieldTaskStore{tasks: map[string]*orchestrator.Task{}}, Actions: &fieldActionStore{}}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?path=status", nil)
	req = withChiURLParam(req, "id", "missing")
	rr := httptest.NewRecorder()
	h.Field(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%q", rr.Code, rr.Body.String())
	}
}
