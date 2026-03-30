package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/hook"
	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/internal/project"
	"github.com/novshi-tech/boid/internal/reducer"
	"github.com/novshi-tech/boid/internal/worktree"
)

type JobHandler struct {
	DB          *db.DB
	Store       *project.Store
	Registry    *reducer.Registry
	Evaluator   *hook.Evaluator
	Runner      *job.Runner
	WorktreeMgr *worktree.Manager
}

func (h *JobHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	r.Post("/{id}/done", h.Done)
	return r
}

func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id query parameter required")
		return
	}
	jobs, err := h.DB.ListJobsByTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*model.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	j, err := h.DB.GetJob(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

type JobDoneRequest struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

func (h *JobHandler) Done(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req JobDoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	j, err := h.DB.GetJob(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Update job status
	if req.ExitCode == 0 {
		j.Status = model.JobStatusCompleted
	} else {
		j.Status = model.JobStatusFailed
	}
	j.ExitCode = req.ExitCode
	j.Output = req.Output

	// Auto-apply action based on exit code
	actionType := "job_completed"
	if req.ExitCode != 0 {
		actionType = "job_failed"
	}

	task, err := h.DB.GetTask(j.TaskID)
	if err != nil {
		slog.Error("job done: task not found", "task_id", j.TaskID)
		writeError(w, http.StatusInternalServerError, "task not found: "+err.Error())
		return
	}

	meta, ok := h.Store.Get(j.ProjectID)
	if !ok {
		slog.Error("job done: project meta not loaded", "project_id", j.ProjectID)
		writeError(w, http.StatusInternalServerError, "project meta not loaded: "+j.ProjectID)
		return
	}

	sm, err := h.Registry.Resolve(meta, task.Behavior)
	if err != nil {
		slog.Error("job done: resolve transition", "error", err)
		writeError(w, http.StatusInternalServerError, "resolve transition: "+err.Error())
		return
	}

	action := &model.Action{TaskID: task.ID, Type: actionType}
	newTask, err := sm.Apply(task, action)
	if err != nil {
		slog.Warn("job done: transition not applicable", "action", actionType, "error", err)
		// Job update still needs to persist even if transition fails
		if err := h.DB.UpdateJob(j); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, j)
		return
	}

	// Persist job update + task transition + action in one transaction
	if err := h.DB.InTx(func(tx *db.Tx) error {
		if err := tx.UpdateJob(j); err != nil {
			return err
		}
		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	slog.Info("job done: auto-applied action", "job_id", j.ID, "action", actionType, "new_status", newTask.Status)

	// Signal any waiting dispatcher that this job is complete
	if h.Runner != nil {
		h.Runner.CompleteJob(j.ID, job.JobCompletionResult{
			Output:   req.Output,
			ExitCode: req.ExitCode,
		})
		h.Runner.UnregisterJob(j.ID)
	}

	// Cleanup worktree on terminal state
	if h.WorktreeMgr != nil {
		if err := h.WorktreeMgr.CleanupForTask(newTask.ID, newTask.Status); err != nil {
			slog.Warn("worktree cleanup failed", "task_id", newTask.ID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, j)
}
