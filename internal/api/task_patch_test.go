package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// patchTaskStore is a minimal TaskService stub for patch handler tests.
type patchTaskService struct {
	task *orchestrator.Task
	err  error
}

func (s *patchTaskService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *patchTaskService) GetTask(id string) (*orchestrator.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.task, nil
}
func (s *patchTaskService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *patchTaskService) UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	t := *s.task
	if req.Title != "" {
		t.Title = req.Title
	}
	if req.Description != "" {
		t.Description = req.Description
	}
	if req.BaseBranch != nil {
		t.BaseBranch = *req.BaseBranch
	}
	if req.BranchPrefix != nil {
		t.BranchPrefix = *req.BranchPrefix
	}
	return &t, nil
}
func (s *patchTaskService) DeleteTask(id string, force bool) error       { return nil }
func (s *patchTaskService) GetTaskDetail(id string) (*TaskDetailView, error) { return nil, nil }
func (s *patchTaskService) ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error) {
	return nil, nil
}
func (s *patchTaskService) DuplicateTask(id string, autoStart bool) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *patchTaskService) RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}

func patchRequest(t *testing.T, handler http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/"+id, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	// inject chi URL param
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestTaskHandlerPatch_TitleOnly(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Title: "old", Description: "desc"}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t1", map[string]any{
		"title": "new title",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestTaskHandlerPatch_PayloadOnly_NoTitleRequired(t *testing.T) {
	task := &orchestrator.Task{ID: "t2", Title: "original", Description: "desc"}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t2", map[string]any{
		"payload": map[string]any{"result": "done"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestTaskHandlerPatch_AllEmpty_ReturnsBadRequest(t *testing.T) {
	task := &orchestrator.Task{ID: "t3", Title: "original"}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t3", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "required") {
		t.Errorf("body %q should mention 'required'", w.Body.String())
	}
}

func TestTaskHandlerPatch_BaseBranchOnly(t *testing.T) {
	task := &orchestrator.Task{ID: "t4", Title: "original"}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t4", map[string]any{
		"base_branch": "master",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var got orchestrator.Task
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BaseBranch != "master" {
		t.Errorf("BaseBranch = %q, want %q", got.BaseBranch, "master")
	}
}

func TestTaskHandlerPatch_BranchPrefixOnly(t *testing.T) {
	task := &orchestrator.Task{ID: "t5", Title: "original"}
	svc := &patchTaskService{task: task}
	h := &TaskHandler{Service: svc}

	w := patchRequest(t, http.HandlerFunc(h.Patch), "t5", map[string]any{
		"branch_prefix": "feature/",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var got orchestrator.Task
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BranchPrefix != "feature/" {
		t.Errorf("BranchPrefix = %q, want %q", got.BranchPrefix, "feature/")
	}
}
