package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// stubScriptService implements ScriptService for testing.
type stubScriptService struct {
	scripts    []orchestrator.Script
	listErr    error
	createdTask *orchestrator.Task
	runErr     error
}

func (s *stubScriptService) ListScripts(projectID string) ([]orchestrator.Script, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.scripts, nil
}

func (s *stubScriptService) RunScript(projectID, kit, scriptID string) (*orchestrator.Task, error) {
	if s.runErr != nil {
		return nil, s.runErr
	}
	return s.createdTask, nil
}

func newTestScriptRouter(svc ScriptService) *chi.Mux {
	h := &ScriptHandler{Service: svc}
	r := chi.NewRouter()
	r.Route("/api/projects/{id}/scripts", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

func TestScriptHandler_List(t *testing.T) {
	scripts := []orchestrator.Script{
		{ID: "notify", Kit: "mykit", Description: "Send notification", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone}},
		{ID: "cleanup", Kit: "mykit", Description: "Cleanup resources"},
	}
	svc := &stubScriptService{scripts: scripts}
	r := newTestScriptRouter(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/projects/proj-1/scripts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got []orchestrator.Script
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(scripts) = %d, want 2", len(got))
	}
	if got[0].ID != "notify" || got[0].Kit != "mykit" {
		t.Errorf("scripts[0] = %+v, want id=notify kit=mykit", got[0])
	}
}

func TestScriptHandler_List_ProjectNotFound(t *testing.T) {
	svc := &stubScriptService{listErr: &StatusError{Code: http.StatusNotFound, Message: "project not found: proj-x"}}
	r := newTestScriptRouter(svc)

	req := httptest.NewRequest(http.MethodGet, "/api/projects/proj-x/scripts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestScriptHandler_Run(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-abc",
		ProjectID: "proj-1",
		Title:     "script: mykit/notify",
		Behavior:  "_script:mykit/notify",
		Ephemeral: true,
	}
	svc := &stubScriptService{createdTask: task}
	r := newTestScriptRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/projects/proj-1/scripts/mykit/notify/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusCreated)
	}

	var got orchestrator.Task
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != "task-abc" {
		t.Errorf("task.ID = %q, want %q", got.ID, "task-abc")
	}
	if !got.Ephemeral {
		t.Error("task.Ephemeral should be true")
	}
}

func TestScriptHandler_Run_ScriptNotFound(t *testing.T) {
	svc := &stubScriptService{runErr: &StatusError{Code: http.StatusNotFound, Message: "script mykit/missing not found"}}
	r := newTestScriptRouter(svc)

	req := httptest.NewRequest(http.MethodPost, "/api/projects/proj-1/scripts/mykit/missing/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestScriptAppService_ListScripts(t *testing.T) {
	scripts := []orchestrator.Script{
		{ID: "deploy", Kit: "ops", Description: "Deploy"},
	}
	meta := &orchestrator.ProjectMeta{Scripts: scripts}
	svc := &ScriptAppService{
		Meta:  stubMetaStore{meta: meta},
		Tasks: &stubTaskStore{},
	}

	got, err := svc.ListScripts("proj-1")
	if err != nil {
		t.Fatalf("ListScripts() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(scripts) = %d, want 1", len(got))
	}
	if got[0].ID != "deploy" {
		t.Errorf("script.ID = %q, want %q", got[0].ID, "deploy")
	}
}

func TestScriptAppService_ListScripts_Empty(t *testing.T) {
	meta := &orchestrator.ProjectMeta{}
	svc := &ScriptAppService{
		Meta:  stubMetaStore{meta: meta},
		Tasks: &stubTaskStore{},
	}

	got, err := svc.ListScripts("proj-1")
	if err != nil {
		t.Fatalf("ListScripts() error = %v", err)
	}
	if got == nil {
		t.Error("ListScripts() returned nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len(scripts) = %d, want 0", len(got))
	}
}

func TestScriptAppService_ListScripts_ProjectNotFound(t *testing.T) {
	svc := &ScriptAppService{
		Meta:  stubMetaStore{meta: nil},
		Tasks: &stubTaskStore{},
	}

	_, err := svc.ListScripts("nonexistent")
	if err == nil {
		t.Fatal("ListScripts() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Errorf("expected 404 StatusError, got %v", err)
	}
}

func TestScriptAppService_RunScript(t *testing.T) {
	script := orchestrator.Script{ID: "deploy", Kit: "ops"}
	meta := &orchestrator.ProjectMeta{Scripts: []orchestrator.Script{script}}
	taskStore := &stubTaskStore{}
	svc := &ScriptAppService{
		Meta:  stubMetaStore{meta: meta},
		Tasks: taskStore,
	}

	task, err := svc.RunScript("proj-1", "ops", "deploy")
	if err != nil {
		t.Fatalf("RunScript() error = %v", err)
	}
	if task == nil {
		t.Fatal("RunScript() returned nil task")
	}
	if taskStore.createdTask == nil {
		t.Fatal("expected task to be created in store")
	}
	if !taskStore.createdTask.Ephemeral {
		t.Error("created task Ephemeral should be true")
	}
	if taskStore.createdTask.ProjectID != "proj-1" {
		t.Errorf("created task ProjectID = %q, want %q", taskStore.createdTask.ProjectID, "proj-1")
	}
}

func TestScriptAppService_RunScript_NotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{Scripts: []orchestrator.Script{
		{ID: "deploy", Kit: "ops"},
	}}
	svc := &ScriptAppService{
		Meta:  stubMetaStore{meta: meta},
		Tasks: &stubTaskStore{},
	}

	_, err := svc.RunScript("proj-1", "ops", "nonexistent")
	if err == nil {
		t.Fatal("RunScript() error = nil, want error")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Errorf("expected 404 StatusError, got %v", err)
	}
}
