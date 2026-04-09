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

// stubWebService is a full implementation of WebService for testing.
type stubWebService struct {
	tasks              []*orchestrator.Task
	taskDetail         *TaskDetailView
	projects           []*orchestrator.Project
	applyActionErr     error
	applyActionCalls   []applyActionCall
	duplicateTaskNewID string
	duplicateTaskErr   error
}

type applyActionCall struct {
	taskID     string
	actionType string
}

func (s *stubWebService) ListTasks(status string) ([]*orchestrator.Task, error) {
	return s.tasks, nil
}

func (s *stubWebService) GetTaskDetail(id string) (*TaskDetailView, error) {
	if s.taskDetail == nil {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return s.taskDetail, nil
}

func (s *stubWebService) ListProjects() ([]*orchestrator.Project, error) {
	return s.projects, nil
}

func (s *stubWebService) ApplyAction(taskID string, actionType string) error {
	s.applyActionCalls = append(s.applyActionCalls, applyActionCall{taskID: taskID, actionType: actionType})
	return s.applyActionErr
}

func (s *stubWebService) DuplicateTask(id string) (string, error) {
	return s.duplicateTaskNewID, s.duplicateTaskErr
}

func (s *stubWebService) ListJobs(status string) ([]JobWithContext, error) {
	return nil, nil
}

func (s *stubWebService) GetJob(id string) (*JobWithContext, error) {
	return nil, fmt.Errorf("job not found: %s", id)
}

// stubWorkflowService implements WorkflowService for WebAppService tests.
type stubWorkflowService struct {
	applyActionErr error
	appliedTaskID  string
	appliedType    string
}

func (s *stubWorkflowService) ApplyAction(ctx context.Context, taskID string, req ApplyActionRequest) (*ActionApplication, error) {
	s.appliedTaskID = taskID
	s.appliedType = req.Type
	if s.applyActionErr != nil {
		return nil, s.applyActionErr
	}
	return &ActionApplication{
		Task:   &orchestrator.Task{ID: taskID},
		Action: &orchestrator.Action{TaskID: taskID, Type: req.Type},
	}, nil
}

func (s *stubWorkflowService) CompleteJob(ctx context.Context, jobID string, req JobDoneRequest) (*Job, error) {
	return nil, nil
}

func (s *stubWorkflowService) TriggerDependents(ctx context.Context, taskID string) {}

func TestWebAppServiceApplyAction_Success(t *testing.T) {
	workflow := &stubWorkflowService{}
	svc := &WebAppService{
		Tasks:    &stubTaskStore{},
		Workflow: workflow,
	}

	if err := svc.ApplyAction("task-1", "start"); err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if workflow.appliedTaskID != "task-1" {
		t.Errorf("appliedTaskID = %q, want %q", workflow.appliedTaskID, "task-1")
	}
	if workflow.appliedType != "start" {
		t.Errorf("appliedType = %q, want %q", workflow.appliedType, "start")
	}
}

func TestWebAppServiceApplyAction_NoWorkflow(t *testing.T) {
	svc := &WebAppService{}

	err := svc.ApplyAction("task-1", "start")
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusInternalServerError {
		t.Fatalf("expected StatusInternalServerError, got %v", err)
	}
}

func TestWebAppServiceApplyAction_WorkflowError(t *testing.T) {
	workflow := &stubWorkflowService{applyActionErr: fmt.Errorf("invalid transition")}
	svc := &WebAppService{
		Tasks:    &stubTaskStore{},
		Workflow: workflow,
	}

	err := svc.ApplyAction("task-1", "start")
	if err == nil {
		t.Fatal("ApplyAction() error = nil, want error")
	}
}

func newTestWebHandler(svc WebService) *chi.Mux {
	h := &WebHandler{Service: svc}
	r := chi.NewRouter()
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	return r
}

func TestWebHandlerPostAction_Success(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{"type": {"start"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/task-1" {
		t.Errorf("Location = %q, want /tasks/task-1", loc)
	}
	if len(svc.applyActionCalls) != 1 {
		t.Fatalf("ApplyAction calls = %d, want 1", len(svc.applyActionCalls))
	}
	if svc.applyActionCalls[0].taskID != "task-1" || svc.applyActionCalls[0].actionType != "start" {
		t.Errorf("ApplyAction call = %+v", svc.applyActionCalls[0])
	}
}

func TestWebHandlerPostAction_MissingType(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandler(svc)

	body := url.Values{}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if len(svc.applyActionCalls) != 0 {
		t.Errorf("ApplyAction should not be called when type is missing")
	}
}

func TestWebHandlerPostAction_ServiceError(t *testing.T) {
	svc := &stubWebService{applyActionErr: fmt.Errorf("cannot apply: wrong status")}
	r := newTestWebHandler(svc)

	body := url.Values{"type": {"abort"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to task detail", loc)
	}
}

func TestWebHandlerPostDuplicate_Success(t *testing.T) {
	svc := &stubWebService{duplicateTaskNewID: "new-task-id"}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/duplicate", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if loc != "/tasks/new-task-id" {
		t.Errorf("Location = %q, want /tasks/new-task-id", loc)
	}
}

func TestWebHandlerPostDuplicate_Error(t *testing.T) {
	svc := &stubWebService{duplicateTaskErr: fmt.Errorf("task not found")}
	r := newTestWebHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/duplicate", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/tasks/task-1") {
		t.Errorf("Location = %q, want redirect to original task", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, want error param", loc)
	}
}

func TestWebAppServiceDuplicateTask_Success(t *testing.T) {
	original := &orchestrator.Task{
		ID:           "orig-id",
		ProjectID:    "proj-1",
		Title:        "My Task",
		Description:  "desc",
		Behavior:     "dev",
		Transition:   "one-shot",
		Traits:       []string{"trait1"},
		Readonly:     false,
		Worktree:     true,
		BranchPrefix: "feature/",
		BaseBranch:   "main",
	}
	store := &stubTaskStore{task: original}
	svc := &WebAppService{Tasks: store}

	newID, err := svc.DuplicateTask("orig-id")
	if err != nil {
		t.Fatalf("DuplicateTask() error = %v", err)
	}
	if newID == "" {
		t.Error("DuplicateTask() returned empty ID")
	}
}

func TestWebAppServiceDuplicateTask_NotFound(t *testing.T) {
	store := &stubTaskStore{err: fmt.Errorf("task not found")}
	svc := &WebAppService{Tasks: store}

	_, err := svc.DuplicateTask("missing-id")
	if err == nil {
		t.Fatal("DuplicateTask() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected StatusNotFound, got %v", err)
	}
}
