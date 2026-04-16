package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// filterTaskService is a TaskService stub that records the filter passed to ListTasks.
type filterTaskService struct {
	capturedFilter orchestrator.TaskFilter
	tasks          []*orchestrator.Task
}

func (s *filterTaskService) CreateTask(req CreateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *filterTaskService) GetTask(id string) (*orchestrator.Task, error) { return nil, nil }
func (s *filterTaskService) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	s.capturedFilter = filter
	if s.tasks == nil {
		return []*orchestrator.Task{}, nil
	}
	return s.tasks, nil
}
func (s *filterTaskService) UpdateTask(id string, req UpdateTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *filterTaskService) DeleteTask(id string, force bool) error { return nil }
func (s *filterTaskService) GetTaskDetail(id string) (*TaskDetailView, error) {
	return nil, nil
}
func (s *filterTaskService) ImportTasks(reqs []CreateTaskRequest) (*ImportResult, error) {
	return nil, nil
}
func (s *filterTaskService) DuplicateTask(id string, autoStart bool) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *filterTaskService) RerunTask(id string, req RerunTaskRequest) (*orchestrator.Task, error) {
	return nil, nil
}

func listFilterRequest(t *testing.T, handler http.Handler, query string) (*httptest.ResponseRecorder, []*orchestrator.Task) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+query, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var tasks []*orchestrator.Task
	if err := json.NewDecoder(rr.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rr, tasks
}

func TestTaskHandler_List_BehaviorFilter(t *testing.T) {
	svc := &filterTaskService{}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?behavior=dev", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if svc.capturedFilter.Behavior != "dev" {
		t.Errorf("captured Behavior = %q, want dev", svc.capturedFilter.Behavior)
	}
}

func TestTaskHandler_List_WorkspaceFilter(t *testing.T) {
	svc := &filterTaskService{}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?workspace_id=ws-1", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if svc.capturedFilter.WorkspaceID != "ws-1" {
		t.Errorf("captured WorkspaceID = %q, want ws-1", svc.capturedFilter.WorkspaceID)
	}
}

func TestTaskHandler_List_HasDependsOnFilter(t *testing.T) {
	svc := &filterTaskService{}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?has_depends_on=true", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !svc.capturedFilter.HasDependsOn {
		t.Errorf("captured HasDependsOn = false, want true")
	}
}

func TestTaskHandler_List_NoDependsOnFilter(t *testing.T) {
	svc := &filterTaskService{}
	h := &TaskHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/?no_depends_on=true", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if !svc.capturedFilter.NoDependsOn {
		t.Errorf("captured NoDependsOn = false, want true")
	}
}
