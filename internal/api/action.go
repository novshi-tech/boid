package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/reducer"
	"github.com/novshi-tech/boid/internal/worktree"
)

type ActionHandler struct {
	DB                  *db.DB
	Store               *project.Store
	Registry            *reducer.Registry
	Evaluator           *hook.Evaluator
	Dispatcher          *hook.Dispatcher          // legacy dispatcher
	AdvancedDispatcher  *hook.AdvancedDispatcher   // new hook→gate→advance dispatcher
	WorktreeMgr         *worktree.Manager
}

func (h *ActionHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Apply)
	return r
}

type ApplyActionRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (h *ActionHandler) Apply(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req ApplyActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}

	// 1. Get task
	task, err := h.DB.GetTask(taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// 2. Get project meta
	meta, ok := h.Store.Get(task.ProjectID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "project meta not loaded: "+task.ProjectID)
		return
	}

	// 3. Resolve state machine
	sm, err := h.Registry.Resolve(meta, task.Behavior)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 4. Apply state transition
	action := &model.Action{
		TaskID:  task.ID,
		Type:    req.Type,
		Payload: req.Payload,
	}

	newTask, err := sm.Apply(task, action)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// 5. Merge payload
	merged, err := model.MergePayload(task.Payload, action.Payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "payload merge: "+err.Error())
		return
	}
	newTask.Payload = merged

	// 6. Save task + action in a transaction
	if err := h.DB.InTx(func(tx *db.Tx) error {
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 7. Cleanup worktree on terminal state
	if h.WorktreeMgr != nil {
		if err := h.WorktreeMgr.CleanupForTask(newTask.ID, newTask.Status); err != nil {
			slog.Warn("worktree cleanup failed", "task_id", newTask.ID, "error", err)
		}
	}

	// 8. Evaluate hooks and dispatch
	matched := h.Evaluator.Evaluate(newTask, meta.Hooks)
	resp := map[string]any{
		"task":          newTask,
		"action":        action,
		"matched_hooks": len(matched),
	}
	if h.Dispatcher != nil && len(matched) > 0 {
		if err := h.Dispatcher.Dispatch(context.Background(), newTask, matched); err != nil {
			resp["dispatch_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
