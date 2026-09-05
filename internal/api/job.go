package api

import (
	"encoding/json"
	"errors"
	"fmt"
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
	LogReader  JobLogReader // optional: enables static GET /{id}/log
	SSEHandler http.Handler // optional: enables SSE streaming for GET /{id}/log?follow=true
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

// writeJobLogUnavailable answers GET /jobs/{id}/log's three distinct 404s
// with a plain-text body that says which one happened.
//
// The three cases used to share one message ("log not available (runtime
// cleaned up)"), which is only true for the third: the other two actively
// mislead — "the id names no job" (passing a task id is a common slip)
// and "this job has no runtime yet" both got reported as a transcript
// that existed and was later swept.
//
// Plain text, not writeError's JSON envelope, for all three: `boid job
// log` prints this body verbatim to the operator (cmd/job.go's
// runJobLog), and a raw {"error":...} blob is a worse read than the
// sentence itself. The status code stays 404 in every case.
func writeJobLogUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(msg + "\n")) //nolint:errcheck
}

// JobLogNoSuchJobMessage, JobLogNoRuntimeMessage and
// JobLogTranscriptGoneMessage are the three explanations above, exported so
// the sandbox-side `boid job log` builtin (internal/server's
// boidBuiltinExecutor) answers with the same words as the REST endpoint.
// An agent reading a log inside a sandbox and an operator reading one at a
// terminal are diagnosing the same thing; having the two surfaces drift into
// separate vocabularies for it is how "log not available" became meaningless
// in the first place.
func JobLogNoSuchJobMessage(id string) string {
	return fmt.Sprintf(
		"no such job %q — if this is a task id, list that task's jobs first: boid job list --task %s",
		id, id)
}

func JobLogNoRuntimeMessage(id string, status JobStatus) string {
	return fmt.Sprintf(
		"log not available: job %q has no runtime (status: %s) — its sandbox never started, so nothing was ever recorded",
		id, status)
}

func JobLogTranscriptGoneMessage(runtimeID string) string {
	return fmt.Sprintf(
		"log not available: runtime %s has no transcript on disk — removed by gc, or lost with the runtimes directory (it lives on tmpfs under the container backend, so a host reboot clears it)",
		runtimeID)
}

func (h *JobHandler) Log(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	j, err := h.Jobs.GetJob(id)
	if err != nil {
		writeJobLogUnavailable(w, JobLogNoSuchJobMessage(id))
		return
	}
	if j.RuntimeID == "" {
		writeJobLogUnavailable(w, JobLogNoRuntimeMessage(id, j.Status))
		return
	}
	data, err := h.LogReader.ReadJobLog(j.RuntimeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJobLogUnavailable(w, JobLogTranscriptGoneMessage(j.RuntimeID))
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

// AgentStop asks the daemon to deliver SIGUSR1 to the runtime pgrp.
// claude.Adapter.Run() catches the signal and forwards SIGTERM to the claude
// child while the surrounding runner-inner-child keeps running and posts
// `boid job done --output-file payload_patch.json` through the broker — that
// callback remains the canonical CompleteJob caller, preserving the agent's
// session id without racing against UnregisterJob. See WorkflowService.StopAgent
// for the lifecycle rationale.
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
