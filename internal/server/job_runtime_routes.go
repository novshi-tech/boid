package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

type resizeJobRuntimeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// mountJobRuntimeRoutes mounts the plain-HTTP job runtime routes; the
// interactive attach stream lives on WebSocket instead (api.WSAttachHandler,
// mounted separately in mountRoutes).
func mountJobRuntimeRoutes(r chi.Router, runtime *appRuntime) {
	if runtime == nil || runtime.jobStore == nil || runtime.runner == nil {
		return
	}

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

		// Routes through Runner (→ SandboxBackend.Adopt → SandboxSession.Resize);
		// the WS "resize" frame (internal/api/ws_attach.go) is the other
		// ingress route through the same backend/session seam.
		if err := runtime.runner.ResizeRuntimeID(req.Context(), job.RuntimeID, dispatcher.TerminalSize{
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
	if job.RuntimeID == "" {
		writeJSONError(w, http.StatusConflict, "job is not attachable")
		return nil, false
	}
	// Routes through Runner (→ SandboxBackend.Adopt) so the backend answers
	// with its own notion of session liveness.
	if !runtime.runner.CanAttach(req.Context(), job.RuntimeID) {
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
