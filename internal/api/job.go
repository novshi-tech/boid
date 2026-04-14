package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
)

type JobHandler struct {
	Jobs      JobStore
	Global    GlobalJobStore // optional: enables cross-task listing when task_id is absent
	Service   WorkflowService
	LogReader JobLogReader // optional: enables GET /{id}/log when set
}

func (h *JobHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	r.Post("/{id}/done", h.Done)
	if h.LogReader != nil {
		r.Get("/{id}/log", h.Log)
	}
	return r
}

func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		h.listGlobal(w, r)
		return
	}
	jobs, err := h.Jobs.ListJobsByTask(taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []*Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *JobHandler) listGlobal(w http.ResponseWriter, r *http.Request) {
	if h.Global == nil {
		writeError(w, http.StatusBadRequest, "task_id query parameter required")
		return
	}
	filter := JobListFilter{
		Status: r.URL.Query().Get("status"),
	}
	if v := r.URL.Query().Get("interactive"); v == "true" {
		t := true
		filter.Interactive = &t
	} else if v == "false" {
		f := false
		filter.Interactive = &f
	}
	jobs, err := h.Global.ListJobsWithContext(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []JobWithContext{}
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

func (h *JobHandler) Log(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	j, err := h.Jobs.GetJob(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if j.RuntimeID == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("log not available (runtime cleaned up)\n")) //nolint:errcheck
		return
	}
	data, err := h.LogReader.ReadJobLog(j.RuntimeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("log not available (runtime cleaned up)\n")) //nolint:errcheck
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(data) //nolint:errcheck
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
