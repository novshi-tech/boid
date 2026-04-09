package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ScriptService defines operations on project scripts.
type ScriptService interface {
	ListScripts(projectID string) ([]orchestrator.Script, error)
	RunScript(projectID, kit, scriptID string) (*orchestrator.Task, error)
}

// ScriptHandler handles HTTP requests for project scripts.
type ScriptHandler struct {
	Service ScriptService
}

func (h *ScriptHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/{kit}/{scriptID}/run", h.Run)
	return r
}

// List handles GET /api/projects/{id}/scripts
func (h *ScriptHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	scripts, err := h.Service.ListScripts(projectID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scripts)
}

// Run handles POST /api/projects/{id}/scripts/{kit}/{scriptID}/run
func (h *ScriptHandler) Run(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	kit := chi.URLParam(r, "kit")
	scriptID := chi.URLParam(r, "scriptID")

	task, err := h.Service.RunScript(projectID, kit, scriptID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// ScriptAppService implements ScriptService using the meta store and task store.
type ScriptAppService struct {
	Meta     MetaStore
	Tasks    TaskStore
	Workflow WorkflowService
}

// ListScripts returns the scripts available for a project.
func (s *ScriptAppService) ListScripts(projectID string) ([]orchestrator.Script, error) {
	meta, ok := s.Meta.Get(projectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: "project not found: " + projectID}
	}
	scripts := meta.Scripts
	if scripts == nil {
		scripts = []orchestrator.Script{}
	}
	return scripts, nil
}

// RunScript creates and starts an ephemeral task for the given script.
func (s *ScriptAppService) RunScript(projectID, kit, scriptID string) (*orchestrator.Task, error) {
	meta, ok := s.Meta.Get(projectID)
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: "project not found: " + projectID}
	}

	var found *orchestrator.Script
	for i := range meta.Scripts {
		if meta.Scripts[i].Kit == kit && meta.Scripts[i].ID == scriptID {
			found = &meta.Scripts[i]
			break
		}
	}
	if found == nil {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("script %s/%s not found", kit, scriptID)}
	}

	payload, _ := json.Marshal(map[string]any{
		"_trigger": map[string]string{"event": "manual"},
	})
	task := orchestrator.BuildScriptTask(*found, projectID, json.RawMessage(payload))

	if err := s.Tasks.CreateTask(task); err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}

	if s.Workflow != nil {
		result, err := s.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("run script: start failed", "script_id", scriptID, "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}

	return task, nil
}
