package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type ScriptHandler struct {
	Meta     MetaStore
	Tasks    TaskStore
	Workflow WorkflowService
}

func (h *ScriptHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/{kit}/{scriptID}/run", h.Run)
	return r
}

func (h *ScriptHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	meta, ok := h.Meta.Get(projectID)
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	scripts := meta.Scripts
	if scripts == nil {
		scripts = []orchestrator.Script{}
	}
	writeJSON(w, http.StatusOK, scripts)
}

func (h *ScriptHandler) Run(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	kit := chi.URLParam(r, "kit")
	scriptID := chi.URLParam(r, "scriptID")

	meta, ok := h.Meta.Get(projectID)
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	var found *orchestrator.Script
	for i := range meta.Scripts {
		if meta.Scripts[i].Kit == kit && meta.Scripts[i].ID == scriptID {
			found = &meta.Scripts[i]
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, "script not found")
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"_trigger": map[string]string{"event": "manual"},
	})

	task := BuildScriptTask(projectID, kit, found, json.RawMessage(payload))
	if err := h.Tasks.CreateTask(task); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if h.Workflow != nil {
		result, err := h.Workflow.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "start"})
		if err != nil {
			slog.Error("script run: failed to start task", "task_id", task.ID, "error", err)
		} else {
			task = result.Task
		}
	}

	writeJSON(w, http.StatusCreated, task)
}

// BuildScriptTask creates an ephemeral Task for running a script manually.
// The payload is set to the provided triggerPayload (e.g. {"_trigger":{"event":"manual"}}).
func BuildScriptTask(projectID, kit string, script *orchestrator.Script, triggerPayload json.RawMessage) *orchestrator.Task {
	title := kit + "/" + script.ID
	return &orchestrator.Task{
		ProjectID:   projectID,
		Title:       title,
		Description: script.Description,
		Behavior:    title,
		Transition:  "one-shot",
		Ephemeral:   true,
		Payload:     triggerPayload,
	}
}
