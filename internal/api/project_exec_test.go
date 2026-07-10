package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubProjectServiceForExec is a minimal ProjectService stub for
// ProjectHandler.StartExec unit tests. Only ResolveProjectRef is exercised
// (StartExec's own dependency, via resolveRef); every other method panics so
// an accidental new dependency shows up as a test failure instead of a
// silently wrong zero value.
type stubProjectServiceForExec struct {
	project *orchestrator.Project
	err     error
}

func (s *stubProjectServiceForExec) CreateProject(string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) ListProjects(string) ([]*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) GetProject(string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) SetProjectWorkspace(string, string) (*orchestrator.Project, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) DeleteProject(string) error { panic("not implemented") }
func (s *stubProjectServiceForExec) ReloadProjects() (*ProjectReloadResult, error) {
	panic("not implemented")
}
func (s *stubProjectServiceForExec) ResolveProjectRef(string) ([]*orchestrator.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []*orchestrator.Project{s.project}, nil
}

// stubExecDispatcher records the request it was called with and returns a
// configured result/error.
type stubExecDispatcher struct {
	called bool
	gotReq StartExecRequest
	result *StartExecResult
	err    error
}

func (s *stubExecDispatcher) StartExec(_ context.Context, req StartExecRequest) (*StartExecResult, error) {
	s.called = true
	s.gotReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func execRequest(t *testing.T, handler http.Handler, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/"+id+"/exec", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestProjectHandlerStartExec_DispatcherNilReturnsNotImplemented(t *testing.T) {
	h := &ProjectHandler{
		Service: &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
	}
	w := execRequest(t, http.HandlerFunc(h.StartExec), "proj-1", map[string]any{"argv": []string{"echo", "hi"}})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusNotImplemented, w.Body.String())
	}
}

func TestProjectHandlerStartExec_EmptyArgvReturnsBadRequest(t *testing.T) {
	dispatcher := &stubExecDispatcher{result: &StartExecResult{JobID: "job-1"}}
	h := &ProjectHandler{
		Service:        &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		ExecDispatcher: dispatcher,
	}
	w := execRequest(t, http.HandlerFunc(h.StartExec), "proj-1", map[string]any{"argv": []string{}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if dispatcher.called {
		t.Error("dispatcher should not be called for empty argv")
	}
}

func TestProjectHandlerStartExec_SuccessPropagatesFieldsAndReturnsCreated(t *testing.T) {
	dispatcher := &stubExecDispatcher{result: &StartExecResult{JobID: "job-1", AttachURL: "/jobs/job-1"}}
	h := &ProjectHandler{
		Service:        &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		ExecDispatcher: dispatcher,
	}
	w := execRequest(t, http.HandlerFunc(h.StartExec), "proj-1", map[string]any{
		"argv":         []string{"go", "test", "./..."},
		"readonly":     true,
		"interactive":  false,
		"display_name": "go test",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
	if !dispatcher.called {
		t.Fatal("expected ExecDispatcher.StartExec to be called")
	}
	if dispatcher.gotReq.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q (must come from the URL, not the body)", dispatcher.gotReq.ProjectID, "proj-1")
	}
	if len(dispatcher.gotReq.Argv) != 3 || dispatcher.gotReq.Argv[0] != "go" {
		t.Errorf("Argv = %v, want [go test ./...]", dispatcher.gotReq.Argv)
	}
	if !dispatcher.gotReq.Readonly {
		t.Error("Readonly = false, want true")
	}
	if dispatcher.gotReq.DisplayName != "go test" {
		t.Errorf("DisplayName = %q, want %q", dispatcher.gotReq.DisplayName, "go test")
	}

	var result StartExecResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.JobID != "job-1" {
		t.Errorf("JobID = %q, want %q", result.JobID, "job-1")
	}
}

func TestProjectHandlerStartExec_UnknownProjectReturnsNotFound(t *testing.T) {
	h := &ProjectHandler{
		Service:        &stubProjectServiceForExec{err: &StatusError{Code: http.StatusNotFound, Message: "not found"}},
		ExecDispatcher: &stubExecDispatcher{},
	}
	w := execRequest(t, http.HandlerFunc(h.StartExec), "missing", map[string]any{"argv": []string{"echo"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestProjectHandlerStartExec_DispatchErrorPropagates(t *testing.T) {
	dispatcher := &stubExecDispatcher{err: &StatusError{Code: http.StatusBadRequest, Message: "cannot resolve default branch"}}
	h := &ProjectHandler{
		Service:        &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		ExecDispatcher: dispatcher,
	}
	w := execRequest(t, http.HandlerFunc(h.StartExec), "proj-1", map[string]any{"argv": []string{"bash"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}
