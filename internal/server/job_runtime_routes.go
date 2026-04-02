package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

type resizeJobRuntimeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

type runtimeAttachSupport interface {
	SupportsAttach(runtimeID string) bool
}

func mountJobRuntimeRoutes(r chi.Router, runtime *appRuntime) {
	if runtime == nil || runtime.jobStore == nil || runtime.jobRuntime == nil {
		return
	}

	r.Post("/api/jobs/{id}/attach", func(w http.ResponseWriter, req *http.Request) {
		job, ok := resolveAttachableJob(w, req, runtime)
		if !ok {
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			writeJSONError(w, http.StatusInternalServerError, "attach is not supported by this server")
			return
		}

		conn, rw, err := hijacker.Hijack()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer conn.Close()

		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: boid-attach\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}

		if err := runtime.jobRuntime.Attach(context.Background(), job.RuntimeID, dispatcher.RuntimeAttachRequest{
			Input:  conn,
			Output: conn,
			Error:  conn,
		}); err != nil && !errors.Is(err, http.ErrAbortHandler) {
			// The transport is already upgraded, so the only useful thing left is logging via stderr.
			_, _ = fmt.Fprintf(conn, "\r\nattach ended: %v\r\n", err)
		}
	})

	r.Post("/api/jobs/{id}/resize", func(w http.ResponseWriter, req *http.Request) {
		job, ok := resolveAttachableJob(w, req, runtime)
		if !ok {
			return
		}

		var body resizeJobRuntimeRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if err := runtime.jobRuntime.Resize(req.Context(), job.RuntimeID, dispatcher.TerminalSize{
			Rows: body.Rows,
			Cols: body.Cols,
		}); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONStatus(w, http.StatusOK, "ok")
	})
}

func resolveAttachableJob(w http.ResponseWriter, req *http.Request, runtime *appRuntime) (*api.Job, bool) {
	jobID := chi.URLParam(req, "id")
	job, err := runtime.jobStore.GetJob(jobID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	if job.RuntimeID == "" || !job.Interactive {
		writeJSONError(w, http.StatusConflict, "job is not attachable")
		return nil, false
	}
	if support, ok := runtime.jobRuntime.(runtimeAttachSupport); ok && !support.SupportsAttach(job.RuntimeID) {
		writeJSONError(w, http.StatusConflict, "job runtime does not support attach")
		return nil, false
	}
	return job, true
}

func writeJSONStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": value})
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
