package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// statusErrorForGetTaskErr classifies a GetTask failure: only "the row
// genuinely doesn't exist" (orchestrator.ErrTaskNotFound) is a 404. Anything
// else (a DB connectivity error, a scan failure, ...) is a 500 — collapsing
// every GetTask error into 404 would report a transient DB outage as "task
// not found", which is misleading and indistinguishable from the real
// not-found case in logs/monitoring.
func statusErrorForGetTaskErr(err error) *StatusError {
	if errors.Is(err, orchestrator.ErrTaskNotFound) {
		return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
	}
	return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
}

// parkPayload is the optional shape of the "park" action's payload:
// {"wake_at": "<RFC3339>", "wake_task_id": "<task id>"}. Both fields are
// optional; a park with no payload still gets a task_triage row (see
// applyParkSideEffect) so ParkedFrom always has somewhere to read from.
type parkPayload struct {
	WakeAt     string `json:"wake_at,omitempty"`
	WakeTaskID string `json:"wake_task_id,omitempty"`
}

// parseParkPayload validates the park action's payload BEFORE the
// transaction opens, so a malformed payload surfaces as 400 rather than
// being swallowed into WithinTx's generic 500 error wrapping.
func parseParkPayload(payload json.RawMessage) (*parkPayload, error) {
	var p parkPayload
	if len(payload) == 0 {
		return &p, nil
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: " + err.Error()}
	}
	if p.WakeAt != "" {
		if _, err := time.Parse(time.RFC3339, p.WakeAt); err != nil {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: wake_at: " + err.Error()}
		}
	}
	return &p, nil
}

// applyParkSideEffect upserts wake_at/wake_task_id into the task_triage
// sidecar as part of the same transaction that records the park action,
// preserving any existing kind/urgency/detail — park's origin status
// itself is NOT duplicated here; it's derived later from the actions log
// via ParkedFrom. p must already be validated via parseParkPayload.
func applyParkSideEffect(tx TxStore, taskID string, p *parkPayload) error {
	// Only "no existing row" should start a fresh sidecar. Any other error
	// (DB connectivity, scan failure, ...) must surface rather than
	// silently blow away an existing row's kind/urgency/detail.
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("park: get task_triage: %w", err)
		}
		tt = &orchestrator.CardAttrs{TaskID: taskID}
	}

	tt.WakeAt = nil
	if p.WakeAt != "" {
		parsed, err := time.Parse(time.RFC3339, p.WakeAt)
		if err != nil {
			// Unreachable in practice (already validated by parseParkPayload),
			// kept defensive since this runs inside a transaction.
			return &StatusError{Code: http.StatusBadRequest, Message: "invalid park payload: wake_at: " + err.Error()}
		}
		tt.WakeAt = &parsed
	}
	tt.WakeTaskID = p.WakeTaskID

	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("park: upsert task_triage: %w", err)
	}
	return nil
}

// childAddedPayload is the shape of the "child_added" action's payload:
// {"id": "<child id>", "title": "<optional>"}.
type childAddedPayload struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

func parseChildAddedPayload(payload json.RawMessage) (*childAddedPayload, error) {
	if len(payload) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_added requires a payload with at least id"}
	}
	var p childAddedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid child_added payload: " + err.Error()}
	}
	if p.ID == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_added payload requires id"}
	}
	return &p, nil
}

// applyChildAddedSideEffect appends a new open child to task_triage.detail
// (idempotent by id — see orchestrator.AddDetailChild). Follows
// applyParkSideEffect's established pattern: p is already validated, the
// read-modify-write on task_triage runs inside the caller's transaction via
// GetTaskTriage (avoiding the race documented on Wake's doc comment).
//
// Enforces the card's single-work-slot invariant for this write port: a
// genuinely NEW child id is rejected (409) while the slot is already
// occupied (cardSlotOccupied — an open/specced JSON child, or a live
// execution task row). A resend of an ALREADY-present id is left to
// AddDetailChild's own idempotent no-op, matching every other
// idempotent-by-id action in this file — it is not a new occupant.
func applyChildAddedSideEffect(tx TxStore, taskID string, p *childAddedPayload) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("child_added: get task_triage: %w", err)
		}
		tt = &orchestrator.CardAttrs{TaskID: taskID}
	}
	existing, cerr := orchestrator.DetailChildren(tt.Detail)
	if cerr != nil {
		return fmt.Errorf("child_added: parse existing children: %w", cerr)
	}
	isResend := false
	for _, c := range existing {
		if c.ID == p.ID {
			isResend = true
			break
		}
	}
	if !isResend {
		occupied, oerr := cardSlotOccupied(tx, taskID)
		if oerr != nil {
			return fmt.Errorf("child_added: check work slot: %w", oerr)
		}
		if occupied {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("child_added: card %q already has an unresolved or in-progress child occupying its single work slot", taskID),
			}
		}
	}
	newDetail, aerr := orchestrator.AddDetailChild(tt.Detail, orchestrator.TaskTriageChild{
		ID:    p.ID,
		Title: p.Title,
	})
	if aerr != nil {
		return fmt.Errorf("child_added: add detail child: %w", aerr)
	}
	tt.Detail = newDetail
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("child_added: upsert task_triage: %w", err)
	}
	return nil
}

// cardSlotOccupied reports whether taskID's single work slot is currently
// occupied: either a live (pending/executing/awaiting) execution task row
// under it — task.OpenChildCount, which counts every non-terminal direct
// child regardless of whether task_triage.detail even mentions it (the
// direct-CreateTask bypass this invariant must also catch) — or an
// open/specced entry in task_triage.detail.children not yet task-ified.
// Must be read inside the same transaction as the write it gates.
func cardSlotOccupied(tx TxStore, cardID string) (bool, error) {
	card, err := tx.GetTask(cardID)
	if err != nil {
		return false, err
	}
	if card.OpenChildCount > 0 {
		return true, nil
	}
	tt, err := tx.GetTaskTriage(cardID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	occupantID, oerr := orchestrator.DetailOpenSlotChildID(tt.Detail)
	if oerr != nil {
		return false, oerr
	}
	return occupantID != "", nil
}

// childDroppedPayload is the shape of the "child_dropped" action's payload:
// {"id": "<child id>", "reason": "<optional>"}. The reason lives in the
// action row only — it is why khi withdrew this child, which the person
// reading the timeline needs and nothing downstream reads.
type childDroppedPayload struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

func parseChildDroppedPayload(payload json.RawMessage) (*childDroppedPayload, error) {
	if len(payload) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_dropped requires a payload with at least id"}
	}
	var p childDroppedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid child_dropped payload: " + err.Error()}
	}
	if p.ID == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_dropped payload requires id"}
	}
	return &p, nil
}

// applyChildDroppedSideEffect closes a child khi decided not to pursue.
// A 409 covers both refusals orchestrator.DropDetailChild makes: an unknown
// id (nothing to drop) and a dispatched child (its task is running — that is
// the daemon's lifecycle to finish). An already-closed child is an
// idempotent no-op so a resend after a lost ack stays harmless.
func applyChildDroppedSideEffect(tx TxStore, taskID string, p *childDroppedPayload) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("child_dropped: get task_triage: %w", err)
		}
		tt = &orchestrator.CardAttrs{TaskID: taskID}
	}
	newDetail, changed, derr := orchestrator.DropDetailChild(tt.Detail, p.ID)
	if derr != nil {
		return &StatusError{Code: http.StatusConflict, Message: "child_dropped: " + derr.Error()}
	}
	if !changed {
		return nil
	}
	tt.Detail = newDetail
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("child_dropped: upsert task_triage: %w", err)
	}
	return nil
}

// childSpeccedPayload is the shape of the "child_specced" action's payload:
// the child id plus its execution recipe (orchestrator.TaskTriageChildSpec's
// fields).
type childSpeccedPayload struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Project     string `json:"project"`
	Behavior    string `json:"behavior,omitempty"`
	Description string `json:"description,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

func parseChildSpeccedPayload(payload json.RawMessage) (*childSpeccedPayload, error) {
	if len(payload) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_specced requires a payload"}
	}
	var p childSpeccedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid child_specced payload: " + err.Error()}
	}
	if p.ID == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_specced payload requires id"}
	}
	if p.Project == "" {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "child_specced payload requires project"}
	}
	return &p, nil
}

// applyChildSpeccedSideEffect advances a previously-added child from open to
// specced, attaching its spec. A child id that doesn't already exist in
// task_triage.detail.children is a 409 (khi is expected to have sent
// child_added for it first — child_specced is update-only, unlike
// child_added's create-or-noop).
func applyChildSpeccedSideEffect(tx TxStore, taskID string, p *childSpeccedPayload) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("child_specced: get task_triage: %w", err)
		}
		tt = &orchestrator.CardAttrs{TaskID: taskID}
	}
	newDetail, serr := orchestrator.SpecDetailChild(tt.Detail, p.ID, orchestrator.TaskTriageChildSpec{
		Project:     p.Project,
		Behavior:    p.Behavior,
		Description: p.Description,
		Instruction: p.Instruction,
	}, p.Title)
	if serr != nil {
		return &StatusError{Code: http.StatusConflict, Message: "child_specced: " + serr.Error()}
	}
	tt.Detail = newDetail
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("child_specced: upsert task_triage: %w", err)
	}
	return nil
}

// promotedAttrVocabulary lists the attrs_set keys the daemon promotes out
// of the opaque detail blob into real task_triage columns, together with
// the closed vocabulary each accepts.
//
// Why these are not opaque like every other attrs key: each one is a real,
// promoted-out column with readers beyond the opaque blob. suggestion_verb
// backs the `/api/cards` read surface (CardView.SuggestionVerb,
// card_read.go — khi's own external contract) and the change-detection
// this same file's notifySuggestionArrived gate keys off (oldVerb
// comparison, below) — neither can read a value that only ever reached
// detail.attrs. kind rides along because it is the same shape (a real
// column, daemon vocabulary, no channel knowledge).
//
// suggestion's promoted list ({go, working, park, drop, done, reopen}) is
// orchestrator.IsCardTransitionAction's own six-verb set restated for this
// map's doc-comment/error-message purposes — the actual gate suggestion
// values pass through is validateSuggestionAttr (suggestion_accept.go),
// which calls IsCardTransitionAction directly (the raw suggestion value is
// a JSON object {verb, reason, params}, not the plain scalar
// parsePromotedAttr's null/string handling expects, so suggestion is never
// routed through parsePromotedAttr the way urgency/kind are). Keep this
// list in sync with IsCardTransitionAction if the card machine's
// transition verbs ever change.
//
// Validating the vocabulary here is not the same as policing "should
// urgency only ever increase", which stays khi's evaluate-side call. This
// is the daemon defending its own read surface: a typo'd urgency (or an
// unknown suggestion.verb) would fail silently with no error surfaced
// anywhere.
var promotedAttrVocabulary = map[string][]string{
	"urgency": {"now", "today", "week", "someday"},
	"kind":    {"signal", "issue", "theme"},
	// start/complete are the current spelling of the retired working/done
	// verbs — validateSuggestionAttr normalizes an incoming legacy spelling
	// to these BEFORE this list is ever consulted, so this closed set only
	// ever needs the current names.
	"suggestion": {"go", "start", "park", "drop", "complete", "reopen"},
}

// attrsSetPatch is the parsed attrs_set payload, split into the opaque keys
// that fold into detail.attrs and the promoted keys that become columns.
type attrsSetPatch struct {
	// Attrs are the opaque keys, folded verbatim (may be empty when the
	// payload carried only promoted keys).
	Attrs map[string]json.RawMessage
	// Urgency/Kind/Verb are set when the payload carried that key; the bool
	// distinguishes "absent" (leave the column alone) from "explicit null"
	// (clear it).
	Urgency    string
	HasUrgency bool
	Kind       string
	HasKind    bool
	// Verb is task_triage.suggestion_verb's promoted value — extracted from
	// the "suggestion" key's {verb, reason, params} object by
	// validateSuggestionAttr, NOT via parsePromotedAttr (that helper expects
	// a plain scalar; suggestion's raw value is a JSON object). Only the verb
	// is promoted — reason/params stay blob-only (see applyAttrsSetSideEffect's
	// doc comment for why the two representations do not drift).
	Verb    string
	HasVerb bool
}

// parsePromotedAttr validates one promoted key's value: a string from the
// closed vocabulary, or JSON null meaning "clear the column".
func parsePromotedAttr(key string, raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid attrs_set payload: %s must be a string or null", key),
		}
	}
	for _, allowed := range promotedAttrVocabulary[key] {
		if value == allowed {
			return value, nil
		}
	}
	return "", &StatusError{
		Code:    http.StatusBadRequest,
		Message: fmt.Sprintf("invalid attrs_set payload: unknown %s %q (allowed: %v)", key, value, promotedAttrVocabulary[key]),
	}
}

// parseAttrsSetPayload validates that the "attrs_set" action's payload is a
// non-empty JSON object BEFORE the transaction opens (same 400-before-Tx
// posture as parseParkPayload) and splits off the promoted keys. Every other
// key's value is opaque to the daemon — see orchestrator.FoldDetailAttrs's doc
// comment for why that part is deliberately a policy-free pass-through.
func parseAttrsSetPayload(payload json.RawMessage) (*attrsSetPatch, error) {
	raw, err := parseAttrsSetObject(payload)
	if err != nil {
		return nil, err
	}
	patch := &attrsSetPatch{Attrs: map[string]json.RawMessage{}}
	for k, v := range raw {
		switch k {
		case "urgency":
			value, perr := parsePromotedAttr(k, v)
			if perr != nil {
				return nil, perr
			}
			patch.Urgency, patch.HasUrgency = value, true
		case "kind":
			value, perr := parsePromotedAttr(k, v)
			if perr != nil {
				return nil, perr
			}
			patch.Kind, patch.HasKind = value, true
		case "suggestion":
			// verb ∈ {go, start, park, drop, complete, reopen} is validated
			// HERE, at attrs_set time, not left opaque like every other attrs
			// key (a retired spelling — working/done — is normalized first;
			// see validateSuggestionAttr's own doc comment). The verb is
			// ALSO promoted to task_triage.suggestion_verb — unlike
			// urgency/kind, the full object still folds into
			// detail.attrs.suggestion via patch.Attrs too (reason/params have
			// no column of their own, and the display side keeps reading the
			// blob), so this key is deliberately written to BOTH places, not
			// promoted-instead-of-folded the way urgency/kind are. See
			// validateSuggestionAttr's own doc comment for why validating
			// (and, now, extracting/normalizing) verb specifically does not
			// cross the workspace-vocabulary boundary this package otherwise
			// protects.
			normalized, verb, verr := validateSuggestionAttr(v)
			if verr != nil {
				return nil, verr
			}
			patch.Attrs[k] = normalized
			patch.Verb, patch.HasVerb = verb, true
		default:
			patch.Attrs[k] = v
		}
	}
	return patch, nil
}

func parseAttrsSetObject(payload json.RawMessage) (map[string]json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "attrs_set requires a non-empty JSON object payload"}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload (must be a JSON object): " + err.Error()}
	}
	// json.Unmarshal happily accepts "null" and "{}" into a nil/empty map
	// without erroring — enforce the documented non-empty-object contract
	// explicitly rather than silently accepting a no-op attrs_set that
	// folds nothing.
	if len(m) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "attrs_set requires a non-empty JSON object payload"}
	}
	return m, nil
}

// applyAttrsSetSideEffect folds the opaque keys into task_triage.detail.attrs
// (last-write-wins, see orchestrator.FoldDetailAttrs) and writes the promoted
// keys to their real columns. Follows applyParkSideEffect's established
// pattern.
//
// urgency/kind are written to their column and NOT also folded into the
// blob: two copies of the same value could drift, and the column is the
// one external readers (the `/api/cards` surface) expect.
//
// suggestion_verb is the one deliberate exception to that rule: only the
// verb is promoted to a column (the readers above only ever need to know
// "does this card have a suggestion", not its full shape), while the full
// suggestion object — including that same verb — stays in
// detail.attrs.suggestion via patch.Attrs too. This is safe from drift
// because there is exactly one writer for both copies (this function, in
// the same call, from the same parsed patch).
//
// Returns verbChanged: whether the promoted column's value actually
// differs from what it held before this call — this function is the only
// place that reads the old value before overwriting it, so it is the
// natural place to answer "should this write also trigger a fresh
// notifySuggestionArrived". false covers both "no verb in this patch at
// all" (HasVerb==false) and "the new verb equals the one already stored":
// a writer that sends unconditionally on every judge cycle must not
// re-notify when nothing actually changed, so the daemon owns the "did
// anything actually change" answer here rather than relying on every
// writer to diff-guard itself.
func applyAttrsSetSideEffect(tx TxStore, taskID string, patch *attrsSetPatch) (verbChanged bool, err error) {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("attrs_set: get task_triage: %w", err)
		}
		tt = &orchestrator.CardAttrs{TaskID: taskID}
	}
	oldVerb := tt.SuggestionVerb
	if len(patch.Attrs) > 0 {
		newDetail, ferr := orchestrator.FoldDetailAttrs(tt.Detail, patch.Attrs)
		if ferr != nil {
			return false, fmt.Errorf("attrs_set: fold detail attrs: %w", ferr)
		}
		tt.Detail = newDetail
	}
	// Strip any stale blob copy of the promoted keys: an older card can
	// still carry detail.attrs.urgency, which would keep reporting the OLD
	// value to every blob reader while the column moves on. Doing this here
	// means the invariant holds for any row that reaches this path
	// regardless of when it was written.
	stripped, sErr := orchestrator.StripDetailAttrs(tt.Detail, "urgency", "kind")
	if sErr != nil {
		return false, fmt.Errorf("attrs_set: strip promoted attrs: %w", sErr)
	}
	tt.Detail = stripped
	if patch.HasUrgency {
		tt.Urgency = patch.Urgency
	}
	if patch.HasKind {
		tt.Kind = patch.Kind
	}
	if patch.HasVerb {
		tt.SuggestionVerb = patch.Verb
	}
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return false, fmt.Errorf("attrs_set: upsert task_triage: %w", err)
	}
	return patch.HasVerb && patch.Verb != oldVerb, nil
}

// parseNotedPayload validates ONLY that the "noted" action's payload is
// syntactically valid JSON. This is deliberately the weakest possible
// check — noted's payload can be any shape (object, array, string, number,
// bool, or null are all legal; unlike attrs_set there is no "must be a
// non-empty object" requirement) and the daemon otherwise never interprets
// a single key inside it. The one thing the daemon does need to guarantee
// is that action_list's JSON-array response stays well-formed — an
// arbitrary non-JSON byte string stored verbatim into actions.payload would
// corrupt every action_list call that has to marshal it back out — so "is
// this valid JSON" is checked here, once, at the write side, rather than
// trusting every future read site to defend against it individually. An
// empty payload is accepted (CreateAction defaults it to "{}", same as
// every other action type).
func parseNotedPayload(payload json.RawMessage) error {
	if len(payload) == 0 {
		return nil
	}
	if !json.Valid(payload) {
		return &StatusError{Code: http.StatusBadRequest, Message: "invalid noted payload: not valid JSON"}
	}
	return nil
}

// answeredPayload is the "answered" action's payload shape:
// {"answer": "accept"|"reject", "verb": "...", "basis": "..."}. answer is
// the one field the daemon validates (closed two-value vocabulary) — it is
// itself a first-class field consumers can switch on, unlike verb/basis
// which are recorded but never interpreted (cross-checking them against
// the suggestion they answer would mean reading the opaque suggestion
// blob — that check belongs to the workspace script or judgment task
// reading action_list, not the daemon).
type answeredPayload struct {
	Answer string `json:"answer"`
	Verb   string `json:"verb,omitempty"`
	Basis  string `json:"basis,omitempty"`
}

// answeredAnswerAccept / answeredAnswerReject are answeredPayload.Answer's
// closed vocabulary.
const (
	answeredAnswerAccept = "accept"
	answeredAnswerReject = "reject"
)

// parseAnsweredPayload validates the "answered" action's payload BEFORE the
// transaction opens, same 400-before-Tx posture as parseParkPayload /
// parseAttrsSetPayload.
func parseAnsweredPayload(payload json.RawMessage) (*answeredPayload, error) {
	if len(payload) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "answered requires a payload with at least answer"}
	}
	var p answeredPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "invalid answered payload: " + err.Error()}
	}
	if p.Answer != answeredAnswerAccept && p.Answer != answeredAnswerReject {
		return nil, &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid answered payload: answer must be %q or %q, got %q", answeredAnswerAccept, answeredAnswerReject, p.Answer),
		}
	}
	return &p, nil
}

// applyAnsweredSideEffect drops detail.attrs.suggestion — the same
// fold-side placement as applyAttrsSetSideEffect's own promoted-key strip
// (a service-layer rewrite of detail outside this fold would give the
// state two writers). Runs unconditionally for BOTH accept and reject:
// either way, the suggestion this answered has been acted on and must
// stop being "the current suggestion" — a fresh one only arrives from a
// fresh note-suggest cycle, which folds a new attrs_set/suggestion in
// from scratch.
//
// A GetTaskTriage miss (sql.ErrNoRows) is a no-op — unlike
// applyAttrsSetSideEffect, which creates an empty row on the same miss.
// `answered` is Manual:true (machine.go), so it can be sent to ANY
// preExecutionStatuses task via `boid action send --type answered`, not
// only triage tasks. Creating a phantom row here for every non-triage task
// that happens to receive one would make an ordinary dev task
// indistinguishable from a real triage task for any future gate keyed on
// "does this task carry a task_triage row". Skipping the row is safe here
// specifically because `answered` has nothing positive to persist when no
// row exists: its only effect is stripping a suggestion key that, with no
// row, cannot be present either. This is deliberately asymmetric with
// attrs_set, whose patch always carries real triage content
// (urgency/kind/suggestion) — for attrs_set, creating the row on miss
// establishes real data, not a bare side effect. See
// TestApplyAction_Answered_NoExistingTriageRow_StillSucceeds.
func applyAnsweredSideEffect(tx TxStore, taskID string) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("answered: get task_triage: %w", err)
		}
		return nil
	}
	stripped, sErr := orchestrator.StripDetailAttrs(tt.Detail, "suggestion")
	if sErr != nil {
		return fmt.Errorf("answered: strip suggestion: %w", sErr)
	}
	tt.Detail = stripped
	// Clear the promoted column alongside the blob key. Every path that
	// strips a suggestion must clear both representations, or a stale
	// value would leak through the `/api/cards` read surface
	// (CardView.SuggestionVerb) and confuse change-detection that keys off it.
	tt.SuggestionVerb = ""
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("answered: upsert task_triage: %w", err)
	}
	return nil
}

// recordAndStripSuggestionIfPresent ensures a direct human card transition
// (go/working/park/drop/done/reopen — every verb IsCardTransitionAction
// admits) that lands while task_triage still carries an existing
// suggestion does not silently discard it: whichever verb the suggestion
// recommended, a DIFFERENT verb committing directly (bypassing
// accept/reject entirely, e.g. a human clicking "drop" while a "park"
// suggestion is pending) supersedes it, and that supersession gets the
// same audit trail accept/reject already record via the "answered" action
// — a new "suggestion_discarded" action, carrying the discarded verb/
// reason and the transition that superseded it.
//
// A GetTaskTriage miss (sql.ErrNoRows) and an absent/malformed suggestion
// are both silent no-ops — mirrors applyAnsweredSideEffect's own miss
// handling immediately above (see its doc comment for why creating a
// phantom row here would be wrong: `transition` is Manual:true on the
// card machine, reachable against any card-carrying task, and a card with
// no task_triage row has nothing to discard).
func recordAndStripSuggestionIfPresent(ctx context.Context, tx TxStore, taskID string, transition *orchestrator.Action) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("record discarded suggestion: get task_triage: %w", err)
	}
	suggestion, ok := orchestrator.DetailSuggestion(tt.Detail)
	if !ok || suggestion.Verb == "" {
		return nil
	}
	payload, mErr := json.Marshal(map[string]string{
		"verb":               suggestion.Verb,
		"reason":             suggestion.Reason,
		"superseding_action": transition.Type,
	})
	if mErr != nil {
		return fmt.Errorf("record discarded suggestion: marshal payload: %w", mErr)
	}
	discard := &orchestrator.Action{
		TaskID:     taskID,
		Type:       "suggestion_discarded",
		FromStatus: transition.FromStatus,
		ToStatus:   transition.ToStatus,
		Payload:    payload,
		Actor:      transition.Actor,
	}
	if err := tx.CreateAction(ctx, discard); err != nil {
		return fmt.Errorf("record discarded suggestion: create action: %w", err)
	}
	stripped, sErr := orchestrator.StripDetailAttrs(tt.Detail, "suggestion")
	if sErr != nil {
		return fmt.Errorf("record discarded suggestion: strip suggestion: %w", sErr)
	}
	tt.Detail = stripped
	// Clear the promoted column too — same reasoning as
	// applyAnsweredSideEffect's own clear, immediately above in this file.
	tt.SuggestionVerb = ""
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("record discarded suggestion: upsert task_triage: %w", err)
	}
	return nil
}

// childResultSummary extracts a closing child's own self-reported
// `payload.artifact.report.summary` (the boid-task skill's canonical report)
// for recordChildClosedOnParent's payload — the one place a live-task-row
// reader (the child's own detail page) and a GC-survivable reader (the
// parent's action log, once the child row itself is gone) can agree on.
// Returns "" for a card (no Exec.Payload at all), a nil/empty payload, or
// any shape that doesn't carry the expected string — never an error, since
// a missing summary must not sink the child_closed record itself.
func childResultSummary(task *orchestrator.Task) string {
	if task.Exec == nil || len(task.Exec.Payload) == 0 {
		return ""
	}
	var p struct {
		Artifact struct {
			Report struct {
				Summary string `json:"summary"`
			} `json:"report"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(task.Exec.Payload, &p); err != nil {
		return ""
	}
	return p.Artifact.Report.Summary
}

// recordChildClosedOnParent is the daemon's own self-record of the
// child_closed vocabulary entry: when a task that is itself a dispatched
// triage child (i.e. some triage parent's task_triage.detail.children[i]
// has TaskRef == task.ID) reaches done/aborted, the daemon — never khi —
// marks that child closed in the parent's task_triage.detail and appends a
// child_closed action to the PARENT's own action log.
//
// Called from finalizeTerminal, the single funnel every terminal-transition
// path (manual done/fail/abort, the dispatch-loop auto-advance, and
// abortOnDispatchError) already routes through — see finalizeTerminal's own
// doc comment ("safe to call multiple times"). This function shares that
// idempotency: orchestrator.MarkDetailChildClosed reports changed=false for
// an already-closed child, and a repeat call here is then a clean no-op
// rather than a duplicate action.
//
// khi can never push child_closed itself: machine.go registers no
// Manual:true rule for the name, so ApplyAction/BoidOpActionSend's
// IsManualAction gate rejects any externally-pushed attempt before it ever
// reaches here (see machine.go's doc comment on the child_dispatched/
// child_closed rules).
func (s *TaskWorkflowService) recordChildClosedOnParent(task *orchestrator.Task) {
	if task == nil || task.ParentID == "" || s.Tx == nil {
		return
	}
	if err := s.Tx.WithinTx(func(tx TxStore) error {
		tt, err := tx.GetTaskTriage(task.ParentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Parent isn't a triage task (e.g. an ordinary
				// supervisor/executor pair) — nothing to record.
				return nil
			}
			return fmt.Errorf("child_closed: get parent task_triage: %w", err)
		}
		newDetail, changed, merr := orchestrator.MarkDetailChildClosed(tt.Detail, task.ID)
		if merr != nil {
			return fmt.Errorf("child_closed: mark detail child closed: %w", merr)
		}
		if !changed {
			return nil
		}
		tt.Detail = newDetail
		if err := tx.UpsertTaskTriage(tt); err != nil {
			return fmt.Errorf("child_closed: upsert parent task_triage: %w", err)
		}
		parentTask, gErr := tx.GetTask(task.ParentID)
		if gErr != nil {
			return fmt.Errorf("child_closed: get parent task: %w", gErr)
		}
		payload, _ := json.Marshal(map[string]string{
			"child_id":      task.ID,
			"child_status":  string(task.Status),
			"child_title":   task.Title,
			"child_project": task.ProjectID,
			"summary":       childResultSummary(task),
		})
		action := &orchestrator.Action{
			TaskID:     task.ParentID,
			Type:       "child_closed",
			FromStatus: parentTask.Status,
			ToStatus:   parentTask.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorDaemon,
		}
		// context.Background(): this self-record is daemon-originated
		// bookkeeping regardless of what triggered finalizeTerminal to run
		// (a sandbox job completing, a dispatch error, ...) — the child_closed
		// FACT itself is always attributed to the daemon (Actor above), never
		// to whatever sandbox write happened to cause it, so there is no
		// TokenContext-carried writer project to thread through here.
		if err := tx.CreateAction(context.Background(), action); err != nil {
			return err
		}
		// A child reaching done/aborted is new judgment material for the
		// parent card — "can I close this now" — so it bumps the parent's
		// updated_at the same way a suggestion attaching does. Gated on
		// `changed` (already true here), matching this whole function's own
		// idempotency: finalizeTerminal may call this repeatedly for the
		// same terminal task, and a retry over an already-closed child must
		// not keep re-bumping updated_at.
		return tx.TouchTaskUpdatedAt(task.ParentID)
	}); err != nil {
		slog.Error("child_closed self-record failed", "task_id", task.ID, "parent_id", task.ParentID, "error", err)
		return
	}
	// A child closing is still recorded (above); whether that means the
	// card itself is done is entirely khi's judgment to suggest and a
	// human's to accept (card machine v2's `done` verb) — the daemon does
	// not evaluate it here or anywhere else.
}

// acceptGo is accept(go)'s implementation: the human-accept path for a "go"
// suggestion, and the direct replacement for v1's two-stage
// ready→(machine "dispatch")→working. v2 has no "ready" status and no
// "dispatch" verb at all — accept(go) does both halves itself, straight
// from parked to working, so this single method IS the whole of Go now.
// A parked card's exits (go/working/drop/done) are all ordinary Manual
// actions, reachable through the same ApplyAction endpoint as everything
// else. See queue_sweep.go's SweepWake for what wake_due became instead
// (a fact record, no transition).
//
// Compensation order (same-Tx is impossible: SetMaxOpenConns(1),
// internal/db/db.go, deadlocks a nested TaskCreator.CreateTask call from
// inside an already-open transaction):
//
//  1. Task-ify every `specced` child in task_triage.detail.children
//     (TaskCreator.CreateTask + auto-start) — non-transactional.
//
//  2. On success, ONE transaction: re-check the task is still parked,
//     apply the "go" transition (parked→working), record
//     child_dispatched actions, persist the children's dispatched
//     status, and — only now, having actually committed the transition —
//     strip the suggestion (applyAnsweredSideEffect). An accept never
//     discards a suggestion it failed to act on.
//
//  3. A failure task-ifying a child (step 1): the card stays parked, the
//     suggestion is left untouched, a dispatch_error action is recorded,
//     and the caller gets a synchronous error. No compensation for any
//     earlier child already created in this same loop (a retry is safe:
//     every child's Ref makes CreateTask idempotent).
//
//  4. A failure in the transition Tx itself, after every child already
//     task-ified successfully: the card stays parked (the failed Tx
//     never committed), suggestion intact, a dispatch_error action is
//     recorded (its payload folds in every already-created child's task
//     id — "orphaned_child_task_ids" — for a human to inspect and
//     hand-abort if warranted), and the caller gets a synchronous error.
//
//     This deliberately does NOT best-effort-abort those children:
//     CreateTask's own (ref, parent_id) get-or-create dedup means "a
//     child THIS call created" is not a reliable fact — a concurrent
//     second accept(go) racing the same card can observe the same
//     already-running child via that dedup, and if THIS call's own
//     transition Tx is the one that loses the race, aborting here would
//     kill the OTHER caller's successfully-dispatched, actually-running
//     child with no error ever surfacing to the caller whose accept
//     genuinely won. Recording the child ids for a human to act on is
//     safer than that failure mode. See recordDispatchError's own doc
//     comment for the same reasoning in code-adjacent form.
//
// A v1 Dispatch failure only ever reached slog.Error, with the "ready"
// action having already committed and the caller already getting an HTTP
// 200. accept(go) failing now always surfaces as a synchronous error to
// the caller, with a dispatch_error action in the audit trail either way.
//
// viaAccept distinguishes the two callers: workflow_action.go's direct
// `req.Type=="go"` early-redirect passes false, applyAnswered's accept(go)
// deferred call (suggestion_accept.go) passes true. When true, the
// suggestion this call is fulfilling was ALREADY recorded as accepted by
// applyAnswered's own "answered" action (committed in a separate, earlier
// Tx) — so stripping it here must NOT also emit a "suggestion_discarded"
// audit action the way a genuinely superseded (different-verb) suggestion
// would: doing so would record the accepted suggestion as if it had been
// thrown away instead. See recordAndStripSuggestionIfPresent's own doc
// comment for the discard-and-record behavior this deliberately bypasses
// when viaAccept is true.
func (s *TaskWorkflowService) acceptGo(ctx context.Context, taskID string, viaAccept bool) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "accept(go): Transactor not configured"}
	}

	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, statusErrorForGetTaskErr(err)
	}
	if task.Status != orchestrator.TaskStatusParked && task.Status != orchestrator.TaskStatusWorking {
		// Append the same rule-table-derived "what CAN be applied from
		// here" hint applyAnswered's generic verb-apply-failure path uses
		// (orchestrator.StateMachine.AvailableActionsHint — single source
		// shared with the Web UI's inapplicable-suggestion notice) — this
		// early status check is go's own equivalent of that generic path
		// (go never reaches sm.Apply directly; see this function's own doc
		// comment), and both a direct "go" click and accept(go) share this
		// message.
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("accept(go): cannot dispatch task in status %q (must be parked or working); %s", task.Status, orchestrator.NewCardMachine().AvailableActionsHint(task.Status)),
		}
	}

	var children []orchestrator.TaskTriageChild
	if s.TaskTriage != nil {
		tt, ttErr := s.TaskTriage.GetTaskTriage(taskID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: "accept(go): get task_triage: " + ttErr.Error()}
			}
			// No task_triage row at all: treat as "no children" — a card can
			// legitimately reach parked with no sidecar row in test/edge
			// scenarios, and a childless accept(go) is still meaningful
			// (bare manual work).
		} else {
			children, err = orchestrator.DetailChildren(tt.Detail)
			if err != nil {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: "accept(go): parse children: " + err.Error()}
			}
		}
	}

	// Validate every child's status up front, before creating anything —
	// same discipline v1's Dispatch established (an unrecognized status must
	// not silently skip past a real data-integrity error).
	for i := range children {
		switch children[i].Status {
		case orchestrator.TaskTriageChildStatusOpen,
			orchestrator.TaskTriageChildStatusSpecced,
			orchestrator.TaskTriageChildStatusDispatched,
			orchestrator.TaskTriageChildStatusClosed:
		default:
			cerr := fmt.Errorf("accept(go): child %q has unrecognized status %q", children[i].ID, children[i].Status)
			s.recordDispatchError(ctx, taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusConflict, Message: cerr.Error()}
		}
	}

	// A card with zero specced children (whether it has no children at all,
	// or every child is still open/already dispatched/closed) has nothing
	// for Go to run — this is a rejection, not a silent no-op transition, so
	// a click that looks like "run the prepared work" never quietly
	// degrades into "just declare working" instead (use Start for that).
	hasSpeccedChild := false
	for i := range children {
		if children[i].Status == orchestrator.TaskTriageChildStatusSpecced {
			hasSpeccedChild = true
			break
		}
	}
	if !hasSpeccedChild {
		cerr := fmt.Errorf("accept(go): no specced child to run — use Start for manual work")
		return nil, &StatusError{Code: http.StatusConflict, Message: cerr.Error()}
	}

	childrenChanged := false
	// newlyDispatched tracks which children THIS call task-ified, both for
	// the child_dispatched audit rows below and for step 4's best-effort
	// abort if the transition Tx itself then fails.
	var newlyDispatched []orchestrator.TaskTriageChild
	for i := range children {
		if children[i].Status != orchestrator.TaskTriageChildStatusSpecced {
			continue
		}
		if children[i].Spec == nil {
			cerr := fmt.Errorf("accept(go): child %q is specced but has no spec", children[i].ID)
			s.recordDispatchError(ctx, taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusConflict, Message: cerr.Error()}
		}
		if s.TaskCreator == nil {
			cerr := fmt.Errorf("accept(go): TaskCreator not configured")
			s.recordDispatchError(ctx, taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
		}
		var instructions json.RawMessage
		if children[i].Spec.Instruction != "" {
			marshaled, mErr := json.Marshal([]orchestrator.Instruction{{Message: children[i].Spec.Instruction}})
			if mErr != nil {
				cerr := fmt.Errorf("accept(go): marshal instruction: %w", mErr)
				s.recordDispatchError(ctx, taskID, task.Status, cerr)
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
			}
			instructions = marshaled
		}
		// Ref: children[i].ID makes this idempotent across a retried
		// accept(go) — same reasoning as v1's Dispatch (CreateTask's own
		// (ref, parent_id) get-or-create dedup, task_create.go).
		childTask, cErr := s.TaskCreator.CreateTask(CreateTaskRequest{
			ProjectID:    children[i].Spec.Project,
			Title:        children[i].Title,
			Description:  children[i].Spec.Description,
			Behavior:     children[i].Spec.Behavior,
			Instructions: instructions,
			ParentID:     taskID,
			Ref:          children[i].ID,
			AutoStart:    true,
		})
		if cErr != nil {
			cerr := fmt.Errorf("accept(go): create child task %q: %w", children[i].ID, cErr)
			s.recordDispatchError(ctx, taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
		}
		if childTask.Status == orchestrator.TaskStatusPending {
			cerr := fmt.Errorf("accept(go): child task %q (%s) was created but failed to auto-start (still pending)", children[i].ID, childTask.ID)
			s.recordDispatchError(ctx, taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
		}
		children[i].Status = orchestrator.TaskTriageChildStatusDispatched
		children[i].TaskRef = childTask.ID
		childrenChanged = true
		newlyDispatched = append(newlyDispatched, children[i])
	}

	sm := orchestrator.NewCardMachine()
	action := &orchestrator.Action{TaskID: taskID, Type: "go", Actor: orchestrator.ActorFromContext(ctx)}
	var newTask *orchestrator.Task
	// concurrentTransitionWon distinguishes one specific failure mode from
	// every other Tx failure below: this exact branch means another
	// accept(go)/direct "go" already committed the parked->working
	// transition before THIS Tx opened. In that specific case,
	// newlyDispatched must NOT be reported as orphaned_child_task_ids (see
	// the txErr handling below) — CreateTask's own (ref, parent_id)
	// get-or-create dedup means these task ids may be the OTHER caller's
	// legitimately-running children, not orphans THIS caller created and
	// abandoned. Any OTHER Tx failure (a genuine write error, not "someone
	// else already won") keeps reporting them, since those really are this
	// call's own unclaimed children.
	var concurrentTransitionWon bool

	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return statusErrorForGetTaskErr(ferr)
		}
		// Re-verify against the status THIS call originally observed (parked
		// or working — both are valid go entry points), not a hardcoded
		// "parked": the working->working self-loop's own fresh read is
		// expected to still say "working", which must not itself look like
		// someone else's concurrent transition.
		if fresh.Status != task.Status {
			concurrentTransitionWon = true
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"accept(go): task status changed to %q before the transition could commit",
					fresh.Status),
			}
		}

		// The working→working self-loop's own concurrency guard: unlike
		// parked→working (where a concurrent winner's transition changes
		// fresh.Status and the check above catches it), working→working
		// commits with fresh.Status staying "working" for every racer, so
		// two concurrent Go calls both re-reading the SAME specced child
		// would otherwise both dispatch it. Re-read task_triage fresh here
		// and confirm every child this call is about to mark dispatched is
		// STILL specced — if a concurrent winner already flipped it to
		// dispatched, this call lost the race.
		var freshTT *orchestrator.CardAttrs
		if len(newlyDispatched) > 0 || childrenChanged {
			var ttErr error
			freshTT, ttErr = tx.GetTaskTriage(taskID)
			if ttErr != nil {
				return fmt.Errorf("accept(go): get task_triage for race check: %w", ttErr)
			}
		}
		if len(newlyDispatched) > 0 {
			freshChildren, fcErr := orchestrator.DetailChildren(freshTT.Detail)
			if fcErr != nil {
				return fmt.Errorf("accept(go): parse fresh children for race check: %w", fcErr)
			}
			freshByID := make(map[string]orchestrator.TaskTriageChild, len(freshChildren))
			for _, fc := range freshChildren {
				freshByID[fc.ID] = fc
			}
			for _, c := range newlyDispatched {
				fc, ok := freshByID[c.ID]
				if !ok || fc.Status != orchestrator.TaskTriageChildStatusSpecced {
					concurrentTransitionWon = true
					return &StatusError{
						Code:    http.StatusConflict,
						Message: fmt.Sprintf("accept(go): child %q was already dispatched by a concurrent request", c.ID),
					}
				}
			}
		}

		applied, applyErr := sm.Apply(fresh, action)
		if applyErr != nil {
			return &StatusError{Code: http.StatusConflict, Message: applyErr.Error()}
		}
		action.FromStatus = fresh.Status
		action.ToStatus = applied.Status

		if err := tx.UpdateTask(applied); err != nil {
			return err
		}
		if err := tx.CreateAction(ctx, action); err != nil {
			return err
		}
		for _, c := range newlyDispatched {
			payload, _ := json.Marshal(map[string]string{"child_id": c.ID, "task_ref": c.TaskRef})
			childAction := &orchestrator.Action{
				TaskID:     taskID,
				Type:       "child_dispatched",
				FromStatus: applied.Status,
				ToStatus:   applied.Status,
				Payload:    payload,
				Actor:      orchestrator.ActorFromContext(ctx),
			}
			if err := tx.CreateAction(ctx, childAction); err != nil {
				return fmt.Errorf("accept(go): record child_dispatched for %q: %w", c.ID, err)
			}
		}
		if childrenChanged {
			newDetail, sErr := orchestrator.SetDetailChildren(freshTT.Detail, children)
			if sErr != nil {
				return sErr
			}
			freshTT.Detail = newDetail
			if err := tx.UpsertTaskTriage(freshTT); err != nil {
				return err
			}
		}
		// Discard any existing suggestion ONLY here, now that the "go"
		// transition has actually committed successfully within this same
		// Tx. Two distinct cases, per viaAccept (this function's own doc
		// comment):
		//
		//   - viaAccept==false (a direct "go" click): a DIFFERENT, unrelated
		//     suggestion may still be sitting in task_triage (e.g. a "park"
		//     suggestion nobody has accepted/rejected yet) — that gets
		//     superseded exactly like every other direct card-transition verb
		//     does under LOW 10, strip AND record (recordAndStripSuggestionIfPresent).
		//   - viaAccept==true (accept(go)): the suggestion THIS call is
		//     fulfilling was already recorded as accepted by applyAnswered's
		//     own "answered" action in an earlier, separate Tx — recording it
		//     again here as "suggestion_discarded" would mislabel the single
		//     most common accept path as a discard (MEDIUM N2). Strip only,
		//     no audit record — applyAnsweredSideEffect is the same
		//     strip-without-recording primitive "answered" itself already
		//     uses for both its accept and reject branches.
		if viaAccept {
			if err := applyAnsweredSideEffect(tx, taskID); err != nil {
				return err
			}
		} else if err := recordAndStripSuggestionIfPresent(ctx, tx, taskID, action); err != nil {
			return err
		}
		newTask = applied
		return nil
	})
	if txErr != nil {
		// Every child above was already successfully created and
		// auto-started before this Tx failed, so their task ids are folded
		// into the dispatch_error payload for a human to inspect — see
		// recordDispatchError's own doc comment for why aborting them here
		// is not done (a concurrent accept(go) racing the same card can,
		// via CreateTask's get-or-create dedup, end up "owning" a child a
		// DIFFERENT caller's Tx actually committed successfully;
		// best-effort-aborting it then kills real in-flight work with no
		// error surfaced to the caller whose accept actually won the
		// race). The card is left parked (this Tx never committed),
		// suggestion intact.
		//
		// When concurrentTransitionWon is true specifically, newlyDispatched
		// is deliberately NOT folded into orphaned_child_task_ids. This
		// losing Tx cannot tell whether these task ids are genuinely THIS
		// call's unclaimed children, or the OTHER (winning) accept's own
		// legitimately-running children that CreateTask's get-or-create
		// dedup happened to hand back to this call too — reporting them
		// here would hand an operator a false lead to hand-abort real,
		// successful work. Every OTHER Tx failure still reports them,
		// since those really are this call's own.
		var orphanedChildTaskIDs []string
		if !concurrentTransitionWon {
			for _, c := range newlyDispatched {
				if c.TaskRef != "" {
					orphanedChildTaskIDs = append(orphanedChildTaskIDs, c.TaskRef)
				}
			}
		}
		s.recordDispatchError(ctx, taskID, task.Status, txErr, orphanedChildTaskIDs...)
		var statusErr *StatusError
		if errors.As(txErr, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: txErr.Error()}
	}

	if s.Hub != nil {
		s.Hub.Broadcast(newTask.ID, TaskEvent{
			Kind: "action",
			Payload: map[string]any{
				"action_id":  action.ID,
				"new_status": string(action.ToStatus),
			},
		})
	}

	// Same post-commit race reconciliation v1's Dispatch always needed: a
	// child fast enough to reach done/aborted before THIS Tx committed its
	// TaskRef into task_triage.detail found no match yet in its own
	// finalizeTerminal→recordChildClosedOnParent call. Re-check and self-heal
	// now that the detail carries every TaskRef.
	for _, c := range newlyDispatched {
		childTask, gErr := s.Tasks.GetTask(c.TaskRef)
		if gErr != nil {
			slog.Error("accept(go): child_closed reconciliation: get child task failed", "child_task_id", c.TaskRef, "error", gErr)
			continue
		}
		if childTask.Status == orchestrator.TaskStatusDone || childTask.Status == orchestrator.TaskStatusAborted {
			s.recordChildClosedOnParent(childTask)
		}
	}

	return &ActionApplication{Task: newTask, Action: action}, nil
}
