package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// SessionHandler exposes the POST /api/sessions surface (and the
// project-scoped /api/projects/{id}/sessions variant mounted by ProjectHandler).
// Phase 3-d (PR1) introduced sessions as a first-class JobKind alongside hook
// and exec so user-initiated agent runs (WebUI [New Session] / `boid agent`)
// no longer have to piggyback on the project command path.
type SessionHandler struct {
	Service    ProjectService
	Dispatcher SessionDispatcher
}

func (h *SessionHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.Start)
	return r
}

// Start handles POST /api/sessions. The request body must specify project_id.
// Use the project-scoped variant when the project is implied by the URL.
//
// Session-id resume was removed: every Start launches a fresh agent process,
// so there is no `/api/sessions/{id}/resume` companion route anymore.
func (h *SessionHandler) Start(w http.ResponseWriter, r *http.Request) {
	var req StartSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}
	h.dispatch(w, r, req)
}

func (h *SessionHandler) dispatch(w http.ResponseWriter, r *http.Request, req StartSessionRequest) {
	if h.Dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, "session dispatcher not wired")
		return
	}
	if msg := validateHarnessType(req.HarnessType); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	result, err := h.Dispatcher.StartSession(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// validateHarnessType returns "" when harness is allowed for a session, or
// the bad-request message otherwise. shell is accepted as the first-class
// "drop me into a sandbox shell" entry point — see `boid agent shell`.
func validateHarnessType(harness string) string {
	switch harness {
	case "claude", "codex", "opencode", "shell":
		return ""
	case "":
		return "harness_type is required (claude / codex / opencode / shell)"
	default:
		return fmt.Sprintf("unsupported harness_type %q (allowed: claude / codex / opencode / shell)", harness)
	}
}
