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

// mountJobRuntimeRoutes mounts the remaining plain-HTTP job runtime routes.
// The interactive attach stream itself moved to WebSocket
// (api.WSAttachHandler, mounted separately in mountRoutes — docs/plans/
// cli-remote-connection.md Phase 3 PR3 "WebSocket attach 一本化"); this file
// used to also own a hand-rolled `POST /api/jobs/{id}/attach` hijack
// handler that spoke a bespoke `Upgrade: boid-attach` protocol
// (internal/client.Client.AttachJob's previous implementation was its only
// caller). PR3 removed both ends outright — two attach transports serving
// the exact same purpose was the maintenance burden decision 5 in the plan
// doc calls out, and the WS route already had to exist for the Web UI.
// /api/jobs/{id}/resize survives unchanged: it is a plain, non-hijacked
// JSON POST unrelated to the attach transport, and stays the CLI's resize
// path (internal/client.Client.ResizeJob, called from cmd/attach.go's
// SIGWINCH handler) — see TestServerJobRuntimeAttachAndResize
// (server_phase3_test.go) for its own regression coverage, independent of
// AttachJob's transport.
func mountJobRuntimeRoutes(r chi.Router, runtime *appRuntime) {
	if runtime == nil || runtime.jobStore == nil || runtime.jobRuntime == nil || runtime.runner == nil {
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

		// Routes through Runner (→ SandboxBackend.Adopt → SandboxSession.Resize)
		// rather than calling runtime.jobRuntime.Resize directly — this is one
		// of the two resize ingress routes docs/plans/phase6-container-backend.md
		// §PR1 requires to go through the backend/session seam (the other is
		// the WS "resize" frame, internal/api/ws_attach.go).
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
	// Routes through Runner (→ SandboxBackend.Adopt) rather than
	// type-asserting runtime.jobRuntime onto a JobRuntime-specific
	// SupportsAttach capability — the pre-Phase-6 check bypassed the
	// SandboxBackend/SandboxSession seam entirely and would give the wrong
	// answer for a runtime whose live session isn't tracked in
	// JobRuntime's own map (a future container backend, PR5). See
	// Runner.CanAttach's doc comment (docs/plans/phase6-container-backend.md
	// §PR1, codex review Blocker 2 on PR #816).
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
