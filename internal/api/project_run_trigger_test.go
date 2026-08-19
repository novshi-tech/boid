package api

// docs/plans/ingestion-identity.md PR-4 (B-5) 12 節「手動 1 巡の口」:
// ProjectHandler.RunTrigger (project.go) の HTTP ハンドラ層テスト。
// stubProjectServiceForExec / execRequest (project_exec_test.go, 同package)
// を流用する — StartExec ハンドラのテストと同じ形。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubTriggerRunner records the (projectID, triggerName) it was called with
// and returns a configured result/error.
type stubTriggerRunner struct {
	called     bool
	gotProject string
	gotTrigger string
	result     *TriggerRunNowResult
	err        error
}

func (s *stubTriggerRunner) RunTriggerNow(_ context.Context, projectID, triggerName string) (*TriggerRunNowResult, error) {
	s.called = true
	s.gotProject = projectID
	s.gotTrigger = triggerName
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func triggerRunRequest(t *testing.T, handler http.Handler, id, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+id+"/triggers/"+name+"/run", nil)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	rctx.URLParams.Add("name", name)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestProjectHandlerRunTrigger_NilRunnerReturnsNotImplemented(t *testing.T) {
	h := &ProjectHandler{
		Service: &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
	}
	w := triggerRunRequest(t, http.HandlerFunc(h.RunTrigger), "proj-1", "intake")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestProjectHandlerRunTrigger_ResolvesProjectAndForwardsTriggerName(t *testing.T) {
	runner := &stubTriggerRunner{result: &TriggerRunNowResult{JobID: "job-1"}}
	h := &ProjectHandler{
		Service:       &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		TriggerRunner: runner,
	}
	w := triggerRunRequest(t, http.HandlerFunc(h.RunTrigger), "proj-1", "intake")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !runner.called {
		t.Fatal("expected TriggerRunner.RunTriggerNow to be called")
	}
	if runner.gotProject != "proj-1" {
		t.Errorf("gotProject = %q, want proj-1 (resolved from ref, not passed through raw)", runner.gotProject)
	}
	if runner.gotTrigger != "intake" {
		t.Errorf("gotTrigger = %q, want intake", runner.gotTrigger)
	}
}

func TestProjectHandlerRunTrigger_UnknownProjectPropagatesServiceError(t *testing.T) {
	h := &ProjectHandler{
		Service:       &stubProjectServiceForExec{err: &StatusError{Code: http.StatusNotFound, Message: "not found"}},
		TriggerRunner: &stubTriggerRunner{},
	}
	w := triggerRunRequest(t, http.HandlerFunc(h.RunTrigger), "missing", "intake")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}

func TestProjectHandlerRunTrigger_RunnerErrorPropagates(t *testing.T) {
	h := &ProjectHandler{
		Service:       &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		TriggerRunner: &stubTriggerRunner{err: &StatusError{Code: http.StatusNotFound, Message: "no trigger named intake"}},
	}
	w := triggerRunRequest(t, http.HandlerFunc(h.RunTrigger), "proj-1", "intake")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}
