package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// stubProjectService implements ProjectService with minimal stubs for the
// execute-endpoint tests. Only ResolveProjectRef is used by the handler
// (via resolveRef); other methods panic to catch unexpected calls.
type stubProjectService struct {
	projects map[string]*orchestrator.Project
}

func (s *stubProjectService) ResolveProjectRef(ref string) ([]*orchestrator.Project, error) {
	if p, ok := s.projects[ref]; ok {
		return []*orchestrator.Project{p}, nil
	}
	return nil, &api.StatusError{Code: http.StatusNotFound, Message: "project not found"}
}

func (s *stubProjectService) CreateProject(string) (*orchestrator.Project, error) { panic("stub") }
func (s *stubProjectService) ListProjects(string) ([]*orchestrator.Project, error) { panic("stub") }
func (s *stubProjectService) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	panic("stub")
}
func (s *stubProjectService) GetProject(string) (*orchestrator.Project, error)           { panic("stub") }
func (s *stubProjectService) SetProjectWorkspace(string, string) (*orchestrator.Project, error) {
	panic("stub")
}
func (s *stubProjectService) DeleteProject(string) error          { panic("stub") }
func (s *stubProjectService) ReloadProjects() (*api.ProjectReloadResult, error) { panic("stub") }
func (s *stubProjectService) GetCommand(string, string) (*api.CommandResponse, error) { panic("stub") }
func (s *stubProjectService) ListCommands(string) ([]api.CommandSummary, error) { panic("stub") }

// stubCommandDispatcher implements CommandDispatcher for testing.
type stubCommandDispatcher struct {
	executeFunc func(ctx context.Context, projectID, commandName string) (*api.ExecuteCommandResult, error)
}

func (s *stubCommandDispatcher) ExecuteCommand(ctx context.Context, projectID, commandName string) (*api.ExecuteCommandResult, error) {
	return s.executeFunc(ctx, projectID, commandName)
}

func newExecuteTestRouter(svc api.ProjectService, disp api.CommandDispatcher) http.Handler {
	h := &api.ProjectHandler{Service: svc, Dispatcher: disp}
	r := chi.NewRouter()
	r.Mount("/api/projects", h.Routes())
	return r
}

func TestProjectExecuteCommand_Success(t *testing.T) {
	svc := &stubProjectService{
		projects: map[string]*orchestrator.Project{
			"exec-proj": {ID: "exec-proj", WorkDir: "/work"},
		},
	}
	disp := &stubCommandDispatcher{
		executeFunc: func(_ context.Context, projectID, commandName string) (*api.ExecuteCommandResult, error) {
			if projectID != "exec-proj" || commandName != "build" {
				t.Errorf("unexpected execute call: projectID=%q commandName=%q", projectID, commandName)
			}
			return &api.ExecuteCommandResult{
				JobID:     "test-job-id",
				AttachURL: "/jobs/test-job-id/terminal",
			}, nil
		},
	}

	rtr := newExecuteTestRouter(svc, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/exec-proj/commands/build/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty response body")
	}
}

func TestProjectExecuteCommand_UnknownProject(t *testing.T) {
	svc := &stubProjectService{projects: map[string]*orchestrator.Project{}}
	disp := &stubCommandDispatcher{
		executeFunc: func(_ context.Context, _, _ string) (*api.ExecuteCommandResult, error) {
			t.Error("dispatcher should not be called for unknown project")
			return nil, nil
		},
	}

	rtr := newExecuteTestRouter(svc, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/no-such-proj/commands/run/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestProjectExecuteCommand_UnknownCommand(t *testing.T) {
	svc := &stubProjectService{
		projects: map[string]*orchestrator.Project{
			"p": {ID: "p", WorkDir: "/work"},
		},
	}
	disp := &stubCommandDispatcher{
		executeFunc: func(_ context.Context, _, _ string) (*api.ExecuteCommandResult, error) {
			return nil, &api.StatusError{Code: http.StatusNotFound, Message: "command \"no-cmd\" not found"}
		},
	}

	rtr := newExecuteTestRouter(svc, disp)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/p/commands/no-cmd/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestProjectExecuteCommand_NoDispatcher(t *testing.T) {
	svc := &stubProjectService{
		projects: map[string]*orchestrator.Project{
			"p": {ID: "p", WorkDir: "/work"},
		},
	}

	rtr := newExecuteTestRouter(svc, nil) // nil dispatcher → 501
	req := httptest.NewRequest(http.MethodPost, "/api/projects/p/commands/run/execute", nil)
	w := httptest.NewRecorder()
	rtr.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

// TestProjectExecuteCommand_Via404Integration verifies that the 404 path works
// end-to-end via the real test server (real MetaStore, real routes).
func TestProjectExecuteCommand_Via404Integration(t *testing.T) {
	ts := testutil.NewTestServer(t)

	// Create a project with a command.
	dir := setupTestProjectWithCommand(t, "exec-int-proj", "Execute Integration Project")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Unknown command name → 404.
	var resp map[string]any
	if err := ts.Client.Do("POST", "/api/projects/exec-int-proj/commands/nonexistent/execute", nil, &resp); err == nil {
		t.Error("expected error for nonexistent command, got nil")
	}
}
