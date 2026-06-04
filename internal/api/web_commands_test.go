package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// stubCmdDispatcher implements CommandDispatcher for testing.
type stubCmdDispatcher struct {
	result *ExecuteCommandResult
	err    error
	calls  []struct{ projectID, commandName, displayName string }
}

func (s *stubCmdDispatcher) ExecuteCommand(ctx context.Context, projectID, commandName, displayName string) (*ExecuteCommandResult, error) {
	s.calls = append(s.calls, struct{ projectID, commandName, displayName string }{projectID, commandName, displayName})
	return s.result, s.err
}

func newTestWebHandlerWithCommands(svc WebService, disp CommandDispatcher) *chi.Mux {
	h := &WebHandler{Service: svc, Dispatcher: disp}
	r := chi.NewRouter()
	r.Post("/projects/{id}/commands/{name}/execute", h.PostProjectExecuteCommand)
	return r
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
	if loc != "/jobs/job-abc" {
		t.Errorf("Location = %q, want /jobs/job-abc", loc)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", len(disp.calls))
	}
	if disp.calls[0].projectID != "proj-1" || disp.calls[0].commandName != "build" {
		t.Errorf("dispatcher call = %+v", disp.calls[0])
	}
}

func TestPostProjectExecuteCommand_PassesDisplayName(t *testing.T) {
	svc := &stubWebService{}
	disp := &stubCmdDispatcher{
		result: &ExecuteCommandResult{JobID: "job-named"},
	}
	r := newTestWebHandlerWithCommands(svc, disp)

	body := strings.NewReader("name=my+session")
	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/build/execute", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", len(disp.calls))
	}
	if disp.calls[0].displayName != "my session" {
		t.Errorf("displayName = %q, want %q", disp.calls[0].displayName, "my session")
	}
}

func TestPostProjectExecuteCommand_EmptyNamePassedThrough(t *testing.T) {
	svc := &stubWebService{}
	disp := &stubCmdDispatcher{
		result: &ExecuteCommandResult{JobID: "job-noname"},
	}
	r := newTestWebHandlerWithCommands(svc, disp)

	req := httptest.NewRequest(http.MethodPost, "/projects/proj-1/commands/shell/execute", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if len(disp.calls) != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", len(disp.calls))
	}
	if disp.calls[0].displayName != "" {
		t.Errorf("displayName = %q, want empty when form field is absent", disp.calls[0].displayName)
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
	if w.Header().Get("HX-Redirect") != "/jobs/job-htmx" {
		t.Errorf("HX-Redirect = %q, want /jobs/job-htmx", w.Header().Get("HX-Redirect"))
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
	if !strings.Contains(loc, "/sessions/new") {
		t.Errorf("Location = %q, should redirect to /sessions/new", loc)
	}
	if !strings.Contains(loc, "error=") {
		t.Errorf("Location = %q, should contain error param", loc)
	}
	if !strings.Contains(loc, "project=proj-1") {
		t.Errorf("Location = %q, should contain project param", loc)
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
