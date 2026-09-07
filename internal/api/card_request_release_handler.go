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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardRequestReleaseStore is the persistence surface CardRequestHandler needs.
type CardRequestReleaseStore interface {
	ForceReleaseCardRequest(id, reason string) ([]orchestrator.ForceReleasedSibling, error)
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

// Release handles POST /api/card-requests/{id}/release. Its response shape
// is ReleaseResult (apiwire_aliases.go) — a daemon↔client wire type, so
// `boid task release-card-request` decodes straight into the SAME type via
// apiwire instead of keeping its own hand-duplicated copy. Body is optional;
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

	// Read before releasing: ForceReleaseCardRequest flips the row's status
	// to failed (target_kind/target_id themselves are left alone), so this
	// is the only chance to learn what status it was in — queued/launching/
	// attached — before that happens. Best-effort — a read failure must not
	// block the release an operator is trying to perform.
	before, _ := h.Store.GetCardRequest(id)

	siblings, err := h.Store.ForceReleaseCardRequest(id, body.Reason)
	if err != nil {
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
	case before != nil && before.Status == orchestrator.CardRequestStatusLaunching &&
		before.LauncherJobID != "" && before.CommandKey == orchestrator.CardRequestCommandKeyGo:
		// A Go reservation's LauncherJobID is a synthetic "go:"+uuid marker,
		// never a real job (workflow_card.go) — there is no `boid job` to
		// inspect. Still tell the operator a card slot was freed while
		// acceptGo may still be mid-flight and could land a continuation
		// against this now-released, force-failed row.
		result.LauncherJobID = before.LauncherJobID
		result.OperatorNotice = fmt.Sprintf(
			"this only freed the card's execution slot — the Go reservation %s has no launcher job to inspect (task creation runs in-process); it may still be mid-flight and could try to attach a continuation to this now-released request",
			before.LauncherJobID)
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
	if len(siblings) > 0 {
		keys := make([]string, len(siblings))
		for i, s := range siblings {
			keys[i] = s.CommandKey
			result.FoldedSiblingsFailed = append(result.FoldedSiblingsFailed, FoldedSiblingSummary{ID: s.ID, CommandKey: s.CommandKey})
		}
		appendOperatorNotice(&result, fmt.Sprintf(
			"this also force-failed %d folded sibling request(s) sharing this card's execution slot (command_key: %s) — fold is scoped to the card, not this request's own command/cause",
			len(siblings), strings.Join(keys, ", ")))
	}
	// ForceReleaseCardRequest unconditionally plants a force-release barrier
	// on the card (SetCardForceReleaseBarrier) — surface it every time, not
	// only alongside a target-specific warning, since it is otherwise
	// invisible to an operator (no CLI/API surface reads it directly).
	appendOperatorNotice(&result, "this card now has a force-release barrier: automatic (event-triggered) card_requests dispatch for it is suppressed until a human operation clears it — a card command, Go, or an explicit retry of a failed request")
	writeJSON(w, http.StatusOK, result)
}

// appendOperatorNotice joins notice onto result.OperatorNotice, separating
// multiple notices with "; " rather than overwriting an earlier one.
func appendOperatorNotice(result *ReleaseResult, notice string) {
	if result.OperatorNotice == "" {
		result.OperatorNotice = notice
	} else {
		result.OperatorNotice += "; " + notice
	}
}
