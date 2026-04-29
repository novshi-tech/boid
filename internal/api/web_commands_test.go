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

// stubCmdDispatcher implements CommandDispatcher for testing.
type stubCmdDispatcher struct {
	result *ExecuteCommandResult
	err    error
	calls  []struct{ projectID, commandName string }
}

func (s *stubCmdDispatcher) ExecuteCommand(ctx context.Context, projectID, commandName string) (*ExecuteCommandResult, error) {
	s.calls = append(s.calls, struct{ projectID, commandName string }{projectID, commandName})
	return s.result, s.err
}

func newTestWebHandlerWithCommands(svc WebService, disp CommandDispatcher) *chi.Mux {
	h := &WebHandler{Service: svc, Dispatcher: disp}
	r := chi.NewRouter()
	r.Get("/projects/{id}/commands", h.ProjectCommandList)
	r.Post("/projects/{id}/commands/{name}/execute", h.PostProjectExecuteCommand)
	return r
}

func TestProjectCommandList_RendersCommands(t *testing.T) {
	svc := &stubWebService{
		projectByID: &orchestrator.Project{
			ID:   "proj-1",
			Meta: orchestrator.ProjectMeta{Name: "My Project"},
		},
		projectCommands: []CommandSummary{
			{Name: "build", Command: []string{"make", "build"}},
			{Name: "test", Command: []string{"go", "test", "./..."}, Readonly: true},
		},
	}
	r := newTestWebHandlerWithCommands(svc, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/commands", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<html") {
		t.Error("should return full HTML page")
	}
	if !strings.Contains(body, "My Project") {
		t.Errorf("should contain project name, got: %s", body[:min(300, len(body))])
	}
	if !strings.Contains(body, "build") {
		t.Error("should contain command name 'build'")
	}
	if !strings.Contains(body, "make build") {
		t.Error("should contain command preview 'make build'")
	}
	if !strings.Contains(body, "readonly") {
		t.Error("should contain readonly badge for 'test' command")
	}
	if !strings.Contains(body, `/projects/proj-1/commands/build/execute`) {
		t.Error("should contain execute form action")
	}
}

func TestProjectCommandList_EmptyCommands(t *testing.T) {
	svc := &stubWebService{
		projectByID: &orchestrator.Project{
			ID:   "proj-1",
			Meta: orchestrator.ProjectMeta{Name: "My Project"},
		},
		projectCommands: []CommandSummary{},
	}
	r := newTestWebHandlerWithCommands(svc, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/commands", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "commands は定義されていません") {
		t.Errorf("should show empty state, got: %s", body[:min(300, len(body))])
	}
}

func TestProjectCommandList_ProjectNotFound(t *testing.T) {
	svc := &stubWebService{
		projectByIDErr: fmt.Errorf("project not found"),
	}
	r := newTestWebHandlerWithCommands(svc, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/no-such/commands", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestProjectCommandList_CommandsError(t *testing.T) {
	svc := &stubWebService{
		projectByID: &orchestrator.Project{
			ID:   "proj-1",
			Meta: orchestrator.ProjectMeta{Name: "My Project"},
		},
		projectCommandsErr: fmt.Errorf("meta not loaded"),
	}
	r := newTestWebHandlerWithCommands(svc, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-1/commands", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error shown as banner)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "meta not loaded") {
		t.Errorf("should show error banner, got: %s", body[:min(300, len(body))])
	}
}

func TestPostProjectExecuteCommand_Success(t *testing.T) {
	svc := &stubWebService{}
	disp := &stubCmdDispatcher{
		result: &ExecuteCommandResult{
			JobID:     "job-abc",
			AttachURL: "/jobs/job-abc/terminal",
		},
	}
	r := newTestWebHandlerWithCommands(svc, disp)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/build/execute", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/jobs/job-abc/terminal" {
		t.Errorf("Location = %q, want /jobs/job-abc/terminal", loc)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", len(disp.calls))
	}
	if disp.calls[0].projectID != "proj-1" || disp.calls[0].commandName != "build" {
		t.Errorf("dispatcher call = %+v", disp.calls[0])
	}
}

func TestPostProjectExecuteCommand_HTMXRedirect(t *testing.T) {
	svc := &stubWebService{}
	disp := &stubCmdDispatcher{
		result: &ExecuteCommandResult{JobID: "job-htmx"},
	}
	r := newTestWebHandlerWithCommands(svc, disp)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/build/execute", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (HX-Redirect)", w.Code)
	}
	if w.Header().Get("HX-Redirect") != "/jobs/job-htmx/terminal" {
		t.Errorf("HX-Redirect = %q, want /jobs/job-htmx/terminal", w.Header().Get("HX-Redirect"))
	}
}

func TestPostProjectExecuteCommand_DispatchError(t *testing.T) {
	svc := &stubWebService{}
	disp := &stubCmdDispatcher{
		err: fmt.Errorf("command not found"),
	}
	r := newTestWebHandlerWithCommands(svc, disp)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/nope/execute", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/projects/proj-1/commands") {
		t.Errorf("Location = %q, should redirect back to commands page", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, should contain error param", loc)
	}
}

func TestPostProjectExecuteCommand_NoDispatcher(t *testing.T) {
	svc := &stubWebService{}
	r := newTestWebHandlerWithCommands(svc, nil)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/build/execute", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func TestProjectCommandList_RouteRegistered(t *testing.T) {
	// Service returns 404 for any project; route must be registered (not chi's 404).
	svc := &stubWebService{projectByIDErr: fmt.Errorf("not found")}
	h := &WebHandler{Service: svc}
	r := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-x/commands", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "404 page not found") {
		t.Error("/projects/{id}/commands route should be registered in WebHandler.Routes()")
	}
}
