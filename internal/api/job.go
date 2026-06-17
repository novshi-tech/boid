package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type JobHandler struct {
	Jobs       JobStore
	Global     GlobalJobStore // optional: enables cross-task listing when task_id is absent
	Service    WorkflowService
	LogReader  JobLogReader  // optional: enables static GET /{id}/log
	SSEHandler http.Handler  // optional: enables SSE streaming for GET /{id}/log?follow=true
}

func (h *JobHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Patch)
	r.Post("/{id}/done", h.Done)
	r.Post("/{id}/agent-stop", h.AgentStop)
	if h.LogReader != nil || h.SSEHandler != nil {
		r.Get("/{id}/log", h.handleLog)
	}
	return r
}

func (h *JobHandler) handleLog(w http.ResponseWriter, r *http.Request) {
	if h.SSEHandler != nil && r.URL.Query().Get("follow") == "true" {
		h.SSEHandler.ServeHTTP(w, r)
		return
	}
	if h.LogReader != nil {
		h.Log(w, r)
		return
	}
	http.NotFound(w, r)
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
	if r.URL.Query().Get("taskless") == "true" {
		filter.TasklessOnly = true
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
	if h.LogReader != nil && j.RuntimeID != "" {
		size, mtime, statErr := h.LogReader.StatJobLog(j.RuntimeID)
		if statErr == nil {
			j.TranscriptSize = size
			j.TranscriptMtime = &mtime
			j.TranscriptIdleSeconds = int64(time.Since(mtime).Seconds())
		} else if !errors.Is(statErr, os.ErrNotExist) {
			// log only; don't fail the response
			_ = statErr
		}
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

type UpdateJobRequest struct {
	DisplayName *string `json:"display_name"`
}

func (h *JobHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req UpdateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DisplayName == nil {
		writeError(w, http.StatusBadRequest, "display_name is required")
		return
	}
	job, err := h.Jobs.GetJob(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	job.DisplayName = strings.TrimSpace(*req.DisplayName)
	if err := h.Jobs.UpdateJob(job); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// AgentStop asks the harness adapter to stop the agent gracefully. bash and
// the EXIT trap survive, so the trap's `boid job done --output-file
// payload_patch.json` remains the canonical CompleteJob caller — preserving
// the agent's session id through the broker token without racing against
// UnregisterJob. See WorkflowService.StopAgent for the lifecycle rationale.
func (h *JobHandler) AgentStop(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Jobs.GetJob(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if job.RuntimeID == "" {
		writeError(w, http.StatusConflict, "job has no runtime to signal")
		return
	}
	h.Service.StopAgent(job.RuntimeID)
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":     job.ID,
		"runtime_id": job.RuntimeID,
		"status":     "signalled",
	})
}
