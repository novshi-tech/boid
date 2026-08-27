package api

// docs/plans/ingestion-identity.md PR-4 (B-5) 12 節「手動 1 巡の口」:
// ProjectHandler.RunTrigger (project.go) の HTTP ハンドラ層テスト。
// stubProjectServiceForExec / execRequest (project_exec_test.go, 同package)
// を流用する — StartExec ハンドラのテストと同じ形。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestProjectHandlerRunTrigger_NameWithSlash_RoundTrips pins the contract a
// signal-derived trigger name relies on: signal_trigger_derive.go names a
// connector-derived trigger "signal:<pack>/<connector>" (e.g.
// "signal:jira-cloud/assigned-issues"), and cmd/trigger.go's `boid trigger
// run` url.PathEscape's it before sending — the exact "/" the routing must
// not choke on. Unlike triggerRunRequest above (which injects the trigger
// name directly into a synthetic chi.RouteContext, bypassing real HTTP path
// parsing entirely), this test drives the handler through a real
// httptest.Server so chi actually matches the request's escaped path — the
// same "chi hands the handler the STILL-ESCAPED form" contract
// TestSecretHandler_KeyWithSlash_GetAndDeleteRoundTrip (secret_test.go)
// already pins for /secrets. Without an explicit url.PathUnescape in
// RunTrigger (matching SecretHandler's own), TriggerRunner sees the literal
// "signal:jira-cloud%2Fassigned-issues" instead of the real trigger name,
// so `boid trigger run` can never fire a derived signal trigger.
func TestProjectHandlerRunTrigger_NameWithSlash_RoundTrips(t *testing.T) {
	const triggerName = "signal:jira-cloud/assigned-issues"
	runner := &stubTriggerRunner{result: &TriggerRunNowResult{JobID: "job-1"}}
	h := &ProjectHandler{
		Service:       &stubProjectServiceForExec{project: &orchestrator.Project{ID: "proj-1"}},
		TriggerRunner: runner,
	}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/proj-1/triggers/"+url.PathEscape(triggerName)+"/run", "application/json", nil)
	if err != nil {
		t.Fatalf("POST .../run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out TriggerRunNowResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !runner.called {
		t.Fatal("expected TriggerRunner.RunTriggerNow to be called")
	}
	if runner.gotTrigger != triggerName {
		t.Errorf("gotTrigger = %q, want %q (unescaped)", runner.gotTrigger, triggerName)
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
