package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/worktree"
)

type ActionHandler struct {
	DB          *db.DB
	Store       *orchestrator.ProjectStore
	Registry    *orchestrator.Registry
	Evaluator   *orchestrator.Evaluator
	Coordinator *orchestrator.Coordinator
	Runner      *dispatcher.Runner
	WorktreeMgr *worktree.Manager
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
	task, err := orchestrator.GetTask(h.DB.Conn, taskID)
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
	action := &orchestrator.Action{
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
	merged, err := project.MergePayload(task.Payload, action.Payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "payload merge: "+err.Error())
		return
	}
	newTask.Payload = merged

	// 6. Save task + action in a transaction
	if err := db.InTxDB(h.DB.Conn, func(tx db.DBTX) error {
		if err := orchestrator.UpdateTask(tx, newTask); err != nil {
			return err
		}
		return orchestrator.CreateAction(tx, action)
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

	// 8. Dispatch hooks and gates asynchronously
	resp := map[string]any{
		"task":   newTask,
		"action": action,
	}

	if h.Coordinator != nil {
		behavior, _ := meta.TaskBehaviors[newTask.Behavior]
		go h.runDispatchLoop(newTask, meta, &behavior, sm)
	}

	writeJSON(w, http.StatusOK, resp)
}

// runDispatchLoop runs the dispatch→advance→re-dispatch loop asynchronously.
// It persists payload and status changes after each cycle.
func (h *ActionHandler) runDispatchLoop(task *orchestrator.Task, meta *project.ProjectMeta, behavior *project.TaskBehavior, sm *orchestrator.StateMachine) {
	const maxCycles = 10
	current := task

	for cycle := 0; cycle < maxCycles; cycle++ {
		result, err := h.Coordinator.DispatchAndAdvance(
			context.Background(), current, meta, behavior, sm,
		)
		if err != nil {
			slog.Error("dispatch loop error", "task_id", current.ID, "cycle", cycle, "error", err)
			return
		}

		// Persist merged payload
		if len(result.FinalPayload) > 0 {
			current.Payload = result.FinalPayload
			if err := db.InTxDB(h.DB.Conn, func(tx db.DBTX) error {
				return orchestrator.UpdateTask(tx, current)
			}); err != nil {
				slog.Error("persist payload failed", "task_id", current.ID, "error", err)
				return
			}
		}

		// If no auto-advance, stop
		if result.NewStatus == "" {
			return
		}

		// Apply the auto-advance
		action := &orchestrator.Action{TaskID: current.ID, Type: "auto_advance"}
		current.Status = result.NewStatus
		if err := db.InTxDB(h.DB.Conn, func(tx db.DBTX) error {
			if err := orchestrator.UpdateTask(tx, current); err != nil {
				return err
			}
			return orchestrator.CreateAction(tx, action)
		}); err != nil {
			slog.Error("auto-advance persist failed", "task_id", current.ID, "error", err)
			return
		}

		slog.Info("auto-advanced", "task_id", current.ID, "new_status", current.Status, "cycle", cycle)

		// Cleanup worktree on terminal state
		if h.WorktreeMgr != nil {
			if err := h.WorktreeMgr.CleanupForTask(current.ID, current.Status); err != nil {
				slog.Warn("worktree cleanup failed", "task_id", current.ID, "error", err)
			}
		}

		// If terminal state, cleanup and stop
		if current.Status == orchestrator.TaskStatusDone || current.Status == orchestrator.TaskStatusAborted {
			if h.Runner != nil {
				h.Runner.CleanupTaskWindow(current.ID)
			}
			return
		}

		// Continue loop: dispatch for the new state
	}

	slog.Warn("dispatch loop max cycles reached", "task_id", current.ID, "max", maxCycles)
}
