package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func setupScriptHandler(meta *orchestrator.ProjectMeta) (*ScriptHandler, *chi.Mux) {
	store := &stubTaskStore{}
	workflow := &stubWorkflowService{}
	h := &ScriptHandler{
		Meta:     stubMetaStore{meta: meta},
		Tasks:    store,
		Workflow: workflow,
	}
	r := chi.NewRouter()
	r.Route("/api/projects/{id}/scripts", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return h, r
}

func TestScriptHandler_List(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Scripts: []orchestrator.Script{
			{ID: "notify", Kit: "boid-kits", Description: "Send notification", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone}},
			{ID: "cleanup", Kit: "boid-kits", Description: "Cleanup resources"},
		},
	}
	_, r := setupScriptHandler(meta)

	req := httptest.NewRequest("GET", "/api/projects/proj-1/scripts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var scripts []orchestrator.Script
	if err := json.NewDecoder(w.Body).Decode(&scripts); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(scripts) != 2 {
		t.Fatalf("len(scripts) = %d, want 2", len(scripts))
	}
	if scripts[0].ID != "notify" || scripts[0].Kit != "boid-kits" {
		t.Errorf("scripts[0] = %+v, want id=notify kit=boid-kits", scripts[0])
	}
}

func TestScriptHandler_List_Empty(t *testing.T) {
	meta := &orchestrator.ProjectMeta{}
	_, r := setupScriptHandler(meta)

	req := httptest.NewRequest("GET", "/api/projects/proj-1/scripts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var scripts []orchestrator.Script
	if err := json.NewDecoder(w.Body).Decode(&scripts); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(scripts) != 0 {
		t.Errorf("expected empty slice, got %d items", len(scripts))
	}
}

func TestScriptHandler_List_ProjectNotFound(t *testing.T) {
	_, r := setupScriptHandler(nil) // nil meta → Get returns false

	req := httptest.NewRequest("GET", "/api/projects/missing/scripts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestScriptHandler_Run(t *testing.T) {
	script := orchestrator.Script{ID: "notify", Kit: "boid-kits", Description: "Send notification"}
	meta := &orchestrator.ProjectMeta{
		Scripts: []orchestrator.Script{script},
	}
	h, r := setupScriptHandler(meta)

	req := httptest.NewRequest("POST", "/api/projects/proj-1/scripts/boid-kits/notify/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify workflow was called with "start"
	wf := h.Workflow.(*stubWorkflowService)
	if wf.appliedType != "start" {
		t.Errorf("ApplyAction type = %q, want %q", wf.appliedType, "start")
	}

	// Verify task fields
	ts := h.Tasks.(*stubTaskStore)
	if ts.createdTask == nil {
		t.Fatal("no task created")
	}
	if ts.createdTask.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", ts.createdTask.ProjectID, "proj-1")
	}
	if ts.createdTask.Title != "boid-kits/notify" {
		t.Errorf("Title = %q, want %q", ts.createdTask.Title, "boid-kits/notify")
	}
	if !ts.createdTask.Ephemeral {
		t.Error("Ephemeral = false, want true")
	}
	if ts.createdTask.Transition != "one-shot" {
		t.Errorf("Transition = %q, want %q", ts.createdTask.Transition, "one-shot")
	}

	// Verify trigger payload
	var payload map[string]any
	if err := json.Unmarshal(ts.createdTask.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	trigger, ok := payload["_trigger"]
	if !ok {
		t.Error("payload missing _trigger key")
	}
	triggerMap, ok := trigger.(map[string]any)
	if !ok || triggerMap["event"] != "manual" {
		t.Errorf("_trigger = %v, want {event: manual}", trigger)
	}
}

func TestScriptHandler_Run_StartFails_TaskStillReturned(t *testing.T) {
	script := orchestrator.Script{ID: "notify", Kit: "boid-kits"}
	meta := &orchestrator.ProjectMeta{Scripts: []orchestrator.Script{script}}
	store := &stubTaskStore{}
	workflow := &stubWorkflowService{applyActionErr: fmt.Errorf("state machine error")}
	h := &ScriptHandler{
		Meta:     stubMetaStore{meta: meta},
		Tasks:    store,
		Workflow: workflow,
	}
	r := chi.NewRouter()
	r.Route("/api/projects/{id}/scripts", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	req := httptest.NewRequest("POST", "/api/projects/proj-1/scripts/boid-kits/notify/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Task is still returned even if start fails
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if store.createdTask == nil {
		t.Fatal("no task created")
	}
}

func TestScriptHandler_Run_ProjectNotFound(t *testing.T) {
	_, r := setupScriptHandler(nil)

	req := httptest.NewRequest("POST", "/api/projects/missing/scripts/boid-kits/notify/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestScriptHandler_Run_ScriptNotFound(t *testing.T) {
	meta := &orchestrator.ProjectMeta{
		Scripts: []orchestrator.Script{
			{ID: "notify", Kit: "boid-kits"},
		},
	}
	_, r := setupScriptHandler(meta)

	req := httptest.NewRequest("POST", "/api/projects/proj-1/scripts/boid-kits/missing/run", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestBuildScriptTask(t *testing.T) {
	script := &orchestrator.Script{
		ID:          "notify",
		Kit:         "boid-kits",
		Description: "Send notification",
	}
	payload := json.RawMessage(`{"_trigger":{"event":"manual"}}`)

	task := BuildScriptTask("proj-1", "boid-kits", script, payload)

	if task.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", task.ProjectID, "proj-1")
	}
	if task.Title != "boid-kits/notify" {
		t.Errorf("Title = %q, want %q", task.Title, "boid-kits/notify")
	}
	if task.Description != "Send notification" {
		t.Errorf("Description = %q, want %q", task.Description, "Send notification")
	}
	if task.Behavior != "boid-kits/notify" {
		t.Errorf("Behavior = %q, want %q", task.Behavior, "boid-kits/notify")
	}
	if task.Transition != "one-shot" {
		t.Errorf("Transition = %q, want %q", task.Transition, "one-shot")
	}
	if !task.Ephemeral {
		t.Error("Ephemeral = false, want true")
	}
}
