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
	// ListActiveCardRequests backs List's bulk mode (no card_id query
	// param): every launching/attached row across every card in one query,
	// so a caller checking many cards (`boid task diagnose-cards`) avoids
	// one GET per card.
	ListActiveCardRequests() ([]*orchestrator.CardRequest, error)
	// GetCardRequest backs Release's pre-release read (see Release's own
	// doc comment for why it needs the row's target BEFORE releasing it).
	GetCardRequest(id string) (*orchestrator.CardRequest, error)
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
// ListCardRequestsByCard's own order). Omitting card_id switches to bulk
// mode: every currently launching/attached row across EVERY card, in one
// query — `boid task diagnose-cards` uses this instead of issuing one GET
// per card.
func (h *CardRequestHandler) List(w http.ResponseWriter, r *http.Request) {
	cardID := r.URL.Query().Get("card_id")
	var rows []*orchestrator.CardRequest
	var err error
	if cardID == "" {
		rows, err = h.Store.ListActiveCardRequests()
	} else {
		rows, err = h.Store.ListCardRequestsByCard(cardID)
	}
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

// ReleaseResult is Release's response shape: it echoes the request's
// pre-release target (if any) since force-release only frees the slot, not
// whatever continuation was still attached to it. Exported so cmd's
// `boid task release-card-request` can decode straight into this type
// instead of keeping its own hand-duplicated copy (a prior duplicate let the
// two silently drift when only one side renamed a field).
type ReleaseResult struct {
	Status     string `json:"status"`
	TargetKind string `json:"target_kind,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	// LauncherJobID is set alongside OperatorNotice for a released row that
	// was still "launching" — its own launcher job, not a task/session
	// continuation, is the thing that may still be running.
	LauncherJobID string `json:"launcher_job_id,omitempty"`
	// HadAttachedTarget reports only that the pre-release row HAD a
	// target_kind/target_id recorded — not that the target is still alive.
	// A task/session that already reached a terminal state and is merely
	// awaiting the next reconcile tick to clear the row also sets this true.
	HadAttachedTarget bool   `json:"had_attached_target,omitempty"`
	OperatorNotice    string `json:"operator_notice,omitempty"`
}

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

	// Read before releasing: ForceReleaseCardRequest clears target_kind/
	// target_id off the row, so this is the only chance to learn what it
	// was pointing at. Best-effort — a read failure must not block the
	// release an operator is trying to perform.
	before, _ := h.Store.GetCardRequest(id)

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
	result := ReleaseResult{Status: "released"}
	switch {
	case before != nil && before.TargetKind != "" && before.TargetID != "":
		result.TargetKind = before.TargetKind
		result.TargetID = before.TargetID
		result.HadAttachedTarget = true
		result.OperatorNotice = fmt.Sprintf(
			"this only freed the card's execution slot — the %s %s it was attached to is NOT stopped and may still be running; stop it by hand if that's not wanted",
			before.TargetKind, before.TargetID)
	case before != nil && before.Status == orchestrator.CardRequestStatusLaunching && before.LauncherJobID != "":
		// A launching row has no continuation attached yet — the thing
		// still possibly running is its OWN launcher job, which force-release
		// never touches. Without this, the operator gets nothing but
		// "released" and no hint that the launcher could still land an
		// AttachCardRequest against this now-released, force-failed row.
		result.LauncherJobID = before.LauncherJobID
		result.OperatorNotice = fmt.Sprintf(
			"this only freed the card's execution slot — launcher job %s is NOT stopped and may still be running (and could still try to attach a continuation to this now-released request); inspect it with `boid job` and stop it by hand if that's not wanted",
			before.LauncherJobID)
	}
	writeJSON(w, http.StatusOK, result)
}
