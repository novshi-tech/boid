package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- TaskAppService: GetTaskBehaviorCommand ---

func TestGetTaskBehaviorCommand_Success(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
	}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				Commands: map[string]orchestrator.CommandSpec{
					"echo-id": {ResolvedCommand: []string{"echo"}, Readonly: true},
				},
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: meta},
	}

	resp, err := svc.GetTaskBehaviorCommand("task-1", "echo-id")
	if err != nil {
		t.Fatalf("GetTaskBehaviorCommand() error = %v", err)
	}
	if len(resp.Command) != 1 || resp.Command[0] != "echo" {
		t.Errorf("Command = %v, want [echo]", resp.Command)
	}
	if !resp.Readonly {
		t.Error("Readonly should be true")
	}
}

func TestGetTaskBehaviorCommand_TaskNotFound(t *testing.T) {
	svc := &TaskAppService{
		Tasks: &stubTaskStore{},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}},
	}
	_, err := svc.GetTaskBehaviorCommand("missing-task", "cmd")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
}

func TestGetTaskBehaviorCommand_MetaNotLoaded(t *testing.T) {
	task := &orchestrator.Task{ID: "t-1", ProjectID: "proj-1", Behavior: "dev"}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: nil},
	}
	_, err := svc.GetTaskBehaviorCommand("t-1", "cmd")
	if err == nil {
		t.Fatal("expected error when meta not loaded")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

func TestGetTaskBehaviorCommand_BehaviorNotFound(t *testing.T) {
	task := &orchestrator.Task{ID: "t-1", ProjectID: "proj-1", Behavior: "missing-behavior"}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: meta},
	}
	_, err := svc.GetTaskBehaviorCommand("t-1", "cmd")
	if err == nil {
		t.Fatal("expected error for missing behavior")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

func TestGetTaskBehaviorCommand_CommandNotFound(t *testing.T) {
	task := &orchestrator.Task{ID: "t-1", ProjectID: "proj-1", Behavior: "dev"}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {Commands: map[string]orchestrator.CommandSpec{}},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: meta},
	}
	_, err := svc.GetTaskBehaviorCommand("t-1", "no-such-cmd")
	if err == nil {
		t.Fatal("expected error for missing command")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("expected 404 StatusError, got %v", err)
	}
}

// --- TaskAppService: ListTaskBehaviorCommands ---

func TestListTaskBehaviorCommands_Success(t *testing.T) {
	task := &orchestrator.Task{ID: "t-1", ProjectID: "proj-1", Behavior: "dev"}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"dev": {
				Commands: map[string]orchestrator.CommandSpec{
					"build":  {ResolvedCommand: []string{"make", "build"}},
					"deploy": {ResolvedCommand: []string{"./deploy.sh"}, Readonly: true},
				},
			},
		},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: meta},
	}

	summaries, err := svc.ListTaskBehaviorCommands("t-1")
	if err != nil {
		t.Fatalf("ListTaskBehaviorCommands() error = %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("got %d summaries, want 2", len(summaries))
	}
	// sorted by name: build before deploy
	if summaries[0].Name != "build" || summaries[1].Name != "deploy" {
		t.Errorf("names = %v %v, want build deploy", summaries[0].Name, summaries[1].Name)
	}
}

func TestListTaskBehaviorCommands_BehaviorNotInMeta(t *testing.T) {
	task := &orchestrator.Task{ID: "t-1", ProjectID: "proj-1", Behavior: "unknown"}
	meta := &orchestrator.ProjectMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{},
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		Meta:  stubMetaStore{meta: meta},
	}
	summaries, err := svc.ListTaskBehaviorCommands("t-1")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected empty slice, got %v", summaries)
	}
}

// --- TaskHandler: GET /tasks/{id}/commands ---

// stubTaskCmdDispatcher implements TaskCommandDispatcher for unit tests.
type stubTaskCmdDispatcher struct {
	summaries  []CommandSummary
	listErr    error
	executeErr error
	executeRes *ExecuteCommandResult
	executedID string
	executedCmd string
}

func (s *stubTaskCmdDispatcher) ListTaskBehaviorCommands(taskID string) ([]CommandSummary, error) {
	return s.summaries, s.listErr
}

func (s *stubTaskCmdDispatcher) ExecuteTaskBehaviorCommand(ctx context.Context, taskID, commandName string) (*ExecuteCommandResult, error) {
	s.executedID = taskID
	s.executedCmd = commandName
	if s.executeErr != nil {
		return nil, s.executeErr
	}
	if s.executeRes != nil {
		return s.executeRes, nil
	}
	return &ExecuteCommandResult{JobID: "job-123", AttachURL: "/jobs/job-123/terminal"}, nil
}

func newTaskCmdTestRouter(svc TaskService, disp TaskCommandDispatcher) http.Handler {
	h := &TaskHandler{Service: svc, Dispatcher: disp}
	r := chi.NewRouter()
	r.Mount("/api/tasks", h.Routes())
	return r
}

func TestTaskHandler_ListTaskCommands_Success(t *testing.T) {
	disp := &stubTaskCmdDispatcher{
		summaries: []CommandSummary{
			{Name: "echo-id", Command: []string{"echo"}, Readonly: true},
		},
	}
	rtr := newTaskCmdTestRouter(nil, disp)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/task-1/commands", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty response body")
	}
}

func TestTaskHandler_ListTaskCommands_NoDispatcher(t *testing.T) {
	rtr := newTaskCmdTestRouter(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/task-1/commands", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty list)", w.Code)
	}
}

func TestTaskHandler_ExecuteTaskCommand_Success(t *testing.T) {
	disp := &stubTaskCmdDispatcher{
		executeRes: &ExecuteCommandResult{JobID: "j-abc", AttachURL: "/jobs/j-abc/terminal"},
	}
	rtr := newTaskCmdTestRouter(nil, disp)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/commands/echo-id/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if disp.executedID != "task-1" || disp.executedCmd != "echo-id" {
		t.Errorf("executed: id=%q cmd=%q, want task-1 echo-id", disp.executedID, disp.executedCmd)
	}
}

func TestTaskHandler_ExecuteTaskCommand_NoDispatcher(t *testing.T) {
	rtr := newTaskCmdTestRouter(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/commands/echo-id/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func TestTaskHandler_ExecuteTaskCommand_NotFound(t *testing.T) {
	disp := &stubTaskCmdDispatcher{
		executeErr: &StatusError{Code: http.StatusNotFound, Message: "command not found"},
	}
	rtr := newTaskCmdTestRouter(nil, disp)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/commands/bad-cmd/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
