package api

// CardRequestHandler serves the operator escape hatch for a stuck
// card_requests row: POST /api/card-requests/{id}/release force-fails a
// queued/launching/attached row regardless of whether its continuation has
// actually terminated (orchestrator.ForceReleaseCardRequest). wire.go
// mounts this directly against runtime.taskRepo, which already implements
// CardRequestReleaseStore — same "no separate service type" shape
// SignalHandler uses for taskRepo above.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardRequestReleaseStore is the narrow persistence surface
// CardRequestHandler needs.
type CardRequestReleaseStore interface {
	ForceReleaseCardRequest(id, reason string) error
}

type CardRequestHandler struct {
	Store CardRequestReleaseStore
}

func (h *CardRequestHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/{id}/release", h.Release)
	return r
}

type releaseCardRequestBody struct {
	Reason string `json:"reason,omitempty"`
}

// releaseReasonMaxBytes bounds the operator-supplied reason recorded onto
// card_requests.error — an operator note, not a payload, so a modest cap is
// enough (unlike `boid agent start --instruction`'s sandbox.PayloadPatchMaxBytes).
const releaseReasonMaxBytes = 4096

// Release handles POST /api/card-requests/{id}/release. Body is optional;
// an empty/missing reason falls back to ForceReleaseCardRequest's own
// default message.
func (h *CardRequestHandler) Release(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "card request id is required")
		return
	}
	var body releaseCardRequestBody
	if r.Body != nil {
		// Best-effort: an empty body is the common case (no reason given),
		// and a malformed one should not block an operator trying to
		// unstick a card — fall back to the default reason rather than 400.
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if len(body.Reason) > releaseReasonMaxBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reason exceeds %d bytes", releaseReasonMaxBytes))
		return
	}

	if err := h.Store.ForceReleaseCardRequest(id, body.Reason); err != nil {
		if errors.Is(err, orchestrator.ErrCardRequestNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, orchestrator.ErrCardRequestInvalidTransition) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}
