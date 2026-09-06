package api

// CardRequestHandler serves the card_requests operator surface:
// GET /api/card-requests?card_id=<id> lists a card's requests (the only way
// to learn a stuck slot's request_id besides re-firing the same command and
// reading its occupied response), and POST /api/card-requests/{id}/release
// force-fails a queued/launching/attached row regardless of whether its
// continuation has actually terminated (orchestrator.ForceReleaseCardRequest).
// wire.go mounts this directly against runtime.taskRepo, which already
// implements CardRequestReleaseStore — same "no separate service type"
// shape SignalHandler uses for taskRepo above.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardRequestReleaseStore is the persistence surface CardRequestHandler needs.
type CardRequestReleaseStore interface {
	ForceReleaseCardRequest(id, reason string) error
	ListCardRequestsByCard(cardID string) ([]*orchestrator.CardRequest, error)
}

type CardRequestHandler struct {
	Store CardRequestReleaseStore
}

func (h *CardRequestHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/{id}/release", h.Release)
	return r
}

// cardRequestView is the wire shape for List — a plain field-by-field
// projection of orchestrator.CardRequest, not the store type itself, so the
// wire contract doesn't shift silently if that type's fields change.
type cardRequestView struct {
	ID            string    `json:"id"`
	CardID        string    `json:"card_id"`
	CommandKey    string    `json:"command_key"`
	CauseID       string    `json:"cause_id,omitempty"`
	Status        string    `json:"status"`
	Instruction   string    `json:"instruction,omitempty"`
	LauncherJobID string    `json:"launcher_job_id,omitempty"`
	TargetKind    string    `json:"target_kind,omitempty"`
	TargetID      string    `json:"target_id,omitempty"`
	FoldedInto    string    `json:"folded_into,omitempty"`
	Result        string    `json:"result,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toCardRequestView(r *orchestrator.CardRequest) cardRequestView {
	return cardRequestView{
		ID: r.ID, CardID: r.CardID, CommandKey: r.CommandKey, CauseID: r.CauseID,
		Status: string(r.Status), Instruction: r.Instruction, LauncherJobID: r.LauncherJobID,
		TargetKind: r.TargetKind, TargetID: r.TargetID, FoldedInto: r.FoldedInto,
		Result: r.Result, Error: r.Error, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// List handles GET /api/card-requests?card_id=<id>, oldest first (matches
// ListCardRequestsByCard's own order). card_id is required — there is no
// bulk cross-card listing yet.
func (h *CardRequestHandler) List(w http.ResponseWriter, r *http.Request) {
	cardID := r.URL.Query().Get("card_id")
	if cardID == "" {
		writeError(w, http.StatusBadRequest, "card_id query parameter is required")
		return
	}
	rows, err := h.Store.ListCardRequestsByCard(cardID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	views := make([]cardRequestView, 0, len(rows))
	for _, row := range rows {
		views = append(views, toCardRequestView(row))
	}
	writeJSON(w, http.StatusOK, views)
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
