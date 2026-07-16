package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeHostCommandsService struct {
	commands  map[string]orchestrator.HostCommandSpec
	reloadErr error
	reloaded  bool
}

func (s *fakeHostCommandsService) HostCommands() map[string]orchestrator.HostCommandSpec {
	return s.commands
}

func (s *fakeHostCommandsService) ReloadHostCommands() error {
	s.reloaded = true
	return s.reloadErr
}

// TestHostCommandsHandler_List pins MINOR 1 (codex review, docs/plans/
// workspace-db-consolidation.md): GET /api/host_commands must return the
// sorted list of names only ("参照名一覧を返す契約"), not the full
// definition map (path/env/policy) — the response is meant for reference-
// name validation (Web UI / CLI checking a workspace's host_commands
// entries), which has no business seeing internal command policy details.
func TestHostCommandsHandler_List(t *testing.T) {
	svc := &fakeHostCommandsService{commands: map[string]orchestrator.HostCommandSpec{
		"gh":  {Allow: []string{"pr"}},
		"aws": {Allow: []string{"s3"}},
	}}
	h := &HostCommandsHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got []string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"aws", "gh"} // sorted
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

// TestHostCommandsHandler_List_Empty verifies an empty aggregated config
// renders as an empty array, not null.
func TestHostCommandsHandler_List_Empty(t *testing.T) {
	svc := &fakeHostCommandsService{commands: map[string]orchestrator.HostCommandSpec{}}
	h := &HostCommandsHandler{Service: svc}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", w.Body.String())
	}
}

func TestHostCommandsHandler_Reload_Success(t *testing.T) {
	svc := &fakeHostCommandsService{commands: map[string]orchestrator.HostCommandSpec{}}
	h := &HostCommandsHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !svc.reloaded {
		t.Error("expected ReloadHostCommands to be called")
	}
}

func TestHostCommandsHandler_Reload_Error(t *testing.T) {
	svc := &fakeHostCommandsService{reloadErr: errParseFailed}
	h := &HostCommandsHandler{Service: svc}

	req := httptest.NewRequest(http.MethodPost, "/reload", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
}

var errParseFailed = &StatusError{Code: http.StatusInternalServerError, Message: "parse failed"}
