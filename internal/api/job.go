package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

type JobHandler struct {
	Jobs    JobStore
	Service *TaskWorkflowService
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
	jobs, err := h.Jobs.ListJobsByTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*dispatcher.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	j, err := h.Jobs.GetJob(id)
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

	job, err := h.Service.CompleteJob(r.Context(), id, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, job)
}
