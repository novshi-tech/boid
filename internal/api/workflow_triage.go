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
// every GetTask error into 404 (as Dispatch originally did) would report a
// transient DB outage as "task not found", which is misleading and, worse,
// indistinguishable from the real not-found case in logs/monitoring (codex
// review round 1, Minor).
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
// preserving any existing kind/urgency/detail. This is the "real writer"
// PR-1's park/wake vertical slice needed (docs/plans/cross-project-issue-triage.md
// Phase 1 PR-1, Opus指摘#1/#12) — park's origin status itself is NOT
// duplicated here; it's derived later from the actions log via ParkedFrom.
// p must already be validated via parseParkPayload.
func applyParkSideEffect(tx TxStore, taskID string, p *parkPayload) error {
	// Only "no existing row" should start a fresh sidecar. Any other error
	// (DB connectivity, scan failure, ...) was previously swallowed the same
	// way, which would silently blow away an existing row's kind/urgency/
	// detail on a transient failure instead of surfacing it (codex review
	// round 1, Minor).
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("park: get task_triage: %w", err)
		}
		tt = &orchestrator.TaskTriage{TaskID: taskID}
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
// {"id": "<child id>", "title": "<optional>"}. Phase 1 PR-4 (docs/plans/
// cross-project-issue-triage.md 論点4/6).
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
func applyChildAddedSideEffect(tx TxStore, taskID string, p *childAddedPayload) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("child_added: get task_triage: %w", err)
		}
		tt = &orchestrator.TaskTriage{TaskID: taskID}
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
// the daemon's lifecycle to finish, 論点9). An already-closed child is an
// idempotent no-op so a resend after a lost ack stays harmless.
func applyChildDroppedSideEffect(tx TxStore, taskID string, p *childDroppedPayload) error {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("child_dropped: get task_triage: %w", err)
		}
		tt = &orchestrator.TaskTriage{TaskID: taskID}
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
// fields). Phase 1 PR-4.
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
		tt = &orchestrator.TaskTriage{TaskID: taskID}
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
// of the opaque detail blob into real task_triage columns, together with the
// closed vocabulary each accepts (docs/plans/cross-project-issue-triage.md
// Phase 1 PR-5a; suggestion added by PR-2, docs/plans/
// suggestion-as-state-transition-impl.md §4.1).
//
// Why these are not opaque like every other attrs key: each one IS a queue
// predicate. ListTasks("queue_next") INNER JOINs task_triage, filters on
// tt.suggestion_verb (the membership gate since PR-2) and orders by
// tt.urgency (an ordering-only attribute since PR-2 — store.go) — so a value
// that only ever reaches detail.attrs can never affect the queue. Before
// PR-5a nothing wrote the urgency column at all, which left the queue view
// permanently empty; the same gap existed for suggestion_verb before PR-2.
// kind rides along because it is the same shape (a real column, daemon
// vocabulary, no channel knowledge).
//
// suggestion differs from urgency/kind in one respect worth flagging here:
// its promoted list ({go, working, park, drop, done, reopen}) is
// orchestrator.IsCardTransitionAction's own six-verb set restated for this
// map's doc-comment/error-message purposes — the actual gate suggestion
// values pass through is validateSuggestionAttr (suggestion_accept.go),
// which calls IsCardTransitionAction directly (the raw suggestion value is a
// JSON OBJECT {verb, reason, params}, not the plain scalar
// parsePromotedAttr's null/string handling expects, so suggestion is never
// routed through parsePromotedAttr the way urgency/kind are — see
// parseAttrsSetPayload's switch below). Keep this list in sync with
// IsCardTransitionAction if the card machine's transition verbs ever change.
//
// Validating the vocabulary here is NOT the policy 逆輸入3/論点6 keep out of the
// daemon (that is "should urgency only ever increase", which stays khi's
// evaluate-side call). This is the daemon defending its own SQL predicate: a
// typo'd urgency (or an unknown suggestion.verb) silently drops the card out
// of the queue forever with no error surfaced anywhere, which is exactly the
// class of silent failure the queue's trust story cannot afford.
var promotedAttrVocabulary = map[string][]string{
	"urgency":    {"now", "today", "week", "someday"},
	"kind":       {"signal", "issue", "theme"},
	"suggestion": {"go", "working", "park", "drop", "done", "reopen"},
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
	// Verb is task_triage.suggestion_verb's promoted value (PR-2, docs/plans/
	// suggestion-as-state-transition-impl.md §4.1) — extracted from the
	// "suggestion" key's {verb, reason, params} object by
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
			// docs/plans/suggestion-as-state-transition-impl.md §3/§4.1: verb
			// ∈ {go, working, park, drop, done, reopen} is validated HERE, at
			// attrs_set time, not left opaque like every other attrs key. The
			// verb is ALSO promoted to task_triage.suggestion_verb (PR-2) —
			// unlike urgency/kind, the full object still folds into
			// detail.attrs.suggestion via patch.Attrs too (reason/params have
			// no column of their own, and the display side keeps reading the
			// blob), so this key is deliberately written to BOTH places, not
			// promoted-instead-of-folded the way urgency/kind are. See
			// validateSuggestionAttr's own doc comment for why validating
			// (and, now, extracting) verb specifically does not cross the
			// workspace-vocabulary boundary J-7 otherwise protects.
			verb, verr := validateSuggestionAttr(v)
			if verr != nil {
				return nil, verr
			}
			patch.Attrs[k] = v
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
	// explicitly (codex review Minor fix) rather than silently accepting a
	// no-op attrs_set that folds nothing.
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
// blob: two copies of the same value could drift, and the column is the one
// the queue SQL actually reads.
//
// suggestion_verb (PR-2, docs/plans/suggestion-as-state-transition-impl.md
// §4.1) is the one deliberate exception to that rule: only the VERB is
// promoted to a column (the queue predicate only ever needs to know
// "does this card have a suggestion", store.go's "queue_next" branch), while
// the full suggestion object — including that same verb — stays in
// detail.attrs.suggestion via patch.Attrs (parseAttrsSetPayload's switch
// folds it there too). This is safe from drift because there is exactly ONE
// writer for both copies (this function, in the same call, from the same
// parsed patch) — unlike urgency/kind, which used to have exactly this
// two-copies problem BEFORE PR-5a promoted them out of the blob entirely.
// verb never needs the same treatment because its blob copy is never
// written independently of the column.
//
// Returns verbChanged (PR #988 review, MEDIUM 3): whether the promoted
// column's value actually differs from what it held before this call — this
// function is the ONLY place that reads the OLD value before overwriting it,
// so it is the natural (and only reliable) place to answer "should this
// write also trigger a fresh notifySuggestionArrived". false covers both "no
// verb in this patch at all" (HasVerb==false) and "the new verb equals the
// one already stored" — the latter is the mechanism-level guard the review
// asked for: khi's own _write_suggestion (write.py) sends unconditionally on
// every judge cycle, unlike its _do_summary/_do_urgency/_do_observed
// siblings, which all diff-guard themselves before writing (the
// _do_observed comment there names the exact 2026-08-20 incident this same
// class of unconditional-rewrite caused). Guarding it here, in boid, means
// neither khi's own fix nor any FUTURE non-khi writer needs to remember to
// diff-guard — the daemon owns the "did anything actually change" answer for
// its own notify trigger.
func applyAttrsSetSideEffect(tx TxStore, taskID string, patch *attrsSetPatch) (verbChanged bool, err error) {
	tt, err := tx.GetTaskTriage(taskID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("attrs_set: get task_triage: %w", err)
		}
		tt = &orchestrator.TaskTriage{TaskID: taskID}
	}
	oldVerb := tt.SuggestionVerb
	if len(patch.Attrs) > 0 {
		newDetail, ferr := orchestrator.FoldDetailAttrs(tt.Detail, patch.Attrs)
		if ferr != nil {
			return false, fmt.Errorf("attrs_set: fold detail attrs: %w", ferr)
		}
		tt.Detail = newDetail
	}
	// Strip any stale blob copy of the promoted keys (Opus review, Medium): a
	// card written before PR-5a can still carry detail.attrs.urgency, which
	// would keep reporting the OLD value to every blob reader while the column
	// the queue SQL reads moves on. Migration 0040 converges the table; doing
	// it here too means the invariant holds for any row that reaches this path
	// regardless.
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
// syntactically valid JSON (docs/plans/ingestion-identity.md PR-3, J-5).
// This is deliberately the weakest possible check — noted's payload is
// "workspace が決める任意の JSON" (any shape: object, array, string, number,
// bool, or null are all legal; unlike attrs_set there is no "must be a
// non-empty object" requirement) and the daemon otherwise never interprets
// a single key inside it. The one thing the daemon DOES need to guarantee
// is that action_list's JSON-array response stays well-formed — an
// arbitrary non-JSON byte string stored verbatim into actions.payload would
// corrupt every action_list call that has to marshal it back out (the
// payload is a json.RawMessage, copied byte-for-byte into the response) —
// so "is this valid JSON" is checked here, once, at the write side, rather
// than trusting every future read site to defend against it individually.
// An empty payload is accepted (CreateAction defaults it to "{}", same as
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

// answeredPayload is the "answered" action's payload shape (J-6):
// {"answer": "accept"|"reject", "verb": "...", "basis": "..."}. answer is
// the one field the daemon validates (closed two-value vocabulary) — it is
// itself a first-class field consumers can switch on, unlike verb/basis
// which are recorded but never interpreted (J-7: cross-checking verb/basis
// against the suggestion they answer would mean reading the opaque
// suggestion blob, crossing the boundary — that check belongs to the
// workspace script or judgment task reading action_list, not the daemon).
type answeredPayload struct {
	Answer string `json:"answer"`
	Verb   string `json:"verb,omitempty"`
	Basis  string `json:"basis,omitempty"`
}

// answeredAnswerAccept / answeredAnswerReject are answeredPayload.Answer's
// closed vocabulary (J-6). khi's real suggestion_answered claims data
// (実測, 2026-08-19) is 25/25 "accept" and 0 "reject" — the design doc calls
// this out explicitly as the reason the reject path needs its own dedicated
// test coverage in this PR, not just the already-exercised accept path.
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

// applyAnsweredSideEffect drops detail.attrs.suggestion (J-6's副作用) — the
// same fold-side placement as applyAttrsSetSideEffect's own promoted-key
// strip (決定13: event 追記が正, state は導出; a service-layer rewrite of
// detail outside this fold would give the state two writers). Runs
// unconditionally for BOTH accept and reject: either way, the suggestion
// this answered has been acted on and must stop being "the current
// suggestion" — a fresh one only arrives from a fresh note-suggest cycle,
// which folds a new attrs_set/suggestion in from scratch.
//
// A GetTaskTriage miss (sql.ErrNoRows) is a no-op — UNLIKE
// applyAttrsSetSideEffect, which creates an empty row on the same miss.
// `answered` is Manual:true (machine.go), so it can be sent to ANY
// preExecutionStatuses task via `boid action send --type answered`, not
// only triage tasks. Creating a phantom row here for every non-triage task
// that happens to receive one would weaken the invariant 12 節 B-6's I-5b
// default plan (docs/plans/ingestion-identity.md) depends on — "service 層
// のガード = task_triage 行の有無を見る" for the future auto-reopen gate
// (PR-5) — by making an ordinary dev task indistinguishable from a real
// triage task. Skipping the row is safe here specifically because
// `answered` has nothing POSITIVE to persist when no row exists: its only
// effect is stripping a suggestion key that, with no row, cannot be
// present either. This is deliberately asymmetric with attrs_set, whose
// patch always carries real triage content (urgency/kind/suggestion) — for
// attrs_set, creating the row on miss establishes real data, not a bare
// side effect, so leaving that path alone here is correct rather than an
// inconsistency (Opus review, 2026-08-19; see
// TestApplyAction_Answered_NoExistingTriageRow_StillSucceeds).
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
	// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1): clear
	// the promoted column alongside the blob key. Every path that strips a
	// suggestion must clear both representations, or the queue predicate
	// (store.go's "queue_next" branch, suggestion_verb != '') would keep
	// showing a card whose suggestion was already answered.
	tt.SuggestionVerb = ""
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("answered: upsert task_triage: %w", err)
	}
	return nil
}

// recordAndStripSuggestionIfPresent implements PR #987 review's LOW 10: a
// direct human card transition (go/working/park/drop/done/reopen — every verb
// IsCardTransitionAction admits) that lands while task_triage still carries an
// existing suggestion must not silently discard it. Before this fix, only
// "go" ever stripped an existing suggestion at all (acceptGo's unconditional
// applyAnsweredSideEffect call below), and even there with no audit trail —
// working/park/drop/done/reopen left a now-stale suggestion sitting in
// detail.attrs untouched. Applied symmetrically to every direct verb now:
// whichever verb the suggestion recommended, a DIFFERENT verb committing
// directly (bypassing accept/reject entirely, e.g. a human clicking "drop"
// while a "park" suggestion is pending) supersedes it, and that supersession
// gets the same audit trail accept/reject already record via the "answered"
// action — a new "suggestion_discarded" action, carrying the discarded verb/
// reason and the transition that superseded it.
//
// A GetTaskTriage miss (sql.ErrNoRows) and an absent/malformed suggestion are
// both silent no-ops — mirrors applyAnsweredSideEffect's own miss handling
// immediately above (see its doc comment for why creating a phantom row here
// would be wrong: `transition` is Manual:true on the card machine, reachable
// against any card-carrying task, and a card with no task_triage row has
// nothing to discard).
func recordAndStripSuggestionIfPresent(tx TxStore, taskID string, transition *orchestrator.Action) error {
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
	if err := tx.CreateAction(discard); err != nil {
		return fmt.Errorf("record discarded suggestion: create action: %w", err)
	}
	stripped, sErr := orchestrator.StripDetailAttrs(tt.Detail, "suggestion")
	if sErr != nil {
		return fmt.Errorf("record discarded suggestion: strip suggestion: %w", sErr)
	}
	tt.Detail = stripped
	// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1): clear
	// the promoted column too — same reasoning as applyAnsweredSideEffect's
	// own clear, immediately above in this file.
	tt.SuggestionVerb = ""
	if err := tx.UpsertTaskTriage(tt); err != nil {
		return fmt.Errorf("record discarded suggestion: upsert task_triage: %w", err)
	}
	return nil
}

// recordChildClosedOnParent is the daemon's own self-record of 論点9's
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
		payload, _ := json.Marshal(map[string]string{"child_id": task.ID, "child_status": string(task.Status)})
		action := &orchestrator.Action{
			TaskID:     task.ParentID,
			Type:       "child_closed",
			FromStatus: parentTask.Status,
			ToStatus:   parentTask.Status,
			Payload:    payload,
			Actor:      orchestrator.ActorDaemon,
		}
		return tx.CreateAction(action)
	}); err != nil {
		slog.Error("child_closed self-record failed", "task_id", task.ID, "parent_id", task.ParentID, "error", err)
		return
	}
	// docs/plans/suggestion-as-state-transition-impl.md §3.4: 決定15's
	// auto-done evaluation used to run right here (autoDoneAfterChildClose,
	// calling into api/triage_done.go's autoDone) — deleted along with
	// SweepTriage. A child closing is still recorded (above); whether that
	// means the CARD itself is done is now entirely khi's judgment to
	// suggest and a human's to accept (card machine v2's `done` verb) — the
	// daemon no longer evaluates it here or anywhere else.
}

// acceptGo is accept(go)'s implementation (docs/plans/
// suggestion-as-state-transition-impl.md §3.3/§B): the human-accept path for
// a "go" suggestion, and the direct replacement for v1's two-stage
// ready→(machine "dispatch")→working. v2 has no "ready" status and no
// "dispatch" verb at all — accept(go) does both halves itself, straight from
// parked to working, so this single method IS the whole of Go now. The old
// Wake mechanism (wake_triaged/wake_ready/wake_working, ParkedFrom-based
// origin resolution) is gone too: v2's card machine has exactly one park
// origin (working), so there is nothing left for a resurfacing step to
// disambiguate — a parked card's exits (go/working/drop) are all ordinary
// Manual actions now, reachable through the same ApplyAction endpoint as
// everything else. See queue_sweep.go's SweepWake for what wake_due became
// instead (a fact record, no transition).
//
// Compensation order (the brief's own explicit instruction — same-Tx is
// impossible: SetMaxOpenConns(1), internal/db/db.go, deadlocks a nested
// TaskCreator.CreateTask call from inside an already-open transaction):
//
//  1. Task-ify every `specced` child in task_triage.detail.children
//     (TaskCreator.CreateTask + auto-start) — NON-transactional, exactly the
//     same constraint v1's Dispatch had.
//
//  2. On success, ONE transaction: re-check the task is still parked, apply
//     the "go" transition (parked→working), record child_dispatched actions,
//     persist the children's dispatched status, and — ONLY NOW, having
//     actually committed the transition — strip the suggestion
//     (applyAnsweredSideEffect). This is the literal reading of design doc
//     §3.1's "遷移を適用してから suggestion を消す": accept never discards a
//     suggestion it failed to act on.
//
//  3. A failure task-ifying a child (step 1): the card stays parked, the
//     suggestion is left untouched (nothing above ever touched it), a
//     dispatch_error action is recorded, and the caller gets a synchronous
//     error. No compensation for any EARLIER child already created in this
//     same loop — same accepted gap v1's Dispatch documented (a retry is
//     safe: every child's Ref makes CreateTask idempotent).
//
//  4. A failure in the transition Tx itself, AFTER every child already
//     task-ified successfully: the card stays parked (the failed Tx never
//     committed), suggestion intact, a dispatch_error action is recorded
//     (its payload folds in every already-created child's task id —
//     "orphaned_child_task_ids" — for a human to inspect and hand-abort if
//     warranted), and the caller gets a synchronous error.
//
//     PR #987 review, BLOCKER 4: an EARLIER version of this step
//     best-effort-ABORTED every child created in step 1, on the theory that
//     a failed transition means they must not be left running unowned. That
//     compensation was removed — CreateTask's own (ref, parent_id)
//     get-or-create dedup (task_create.go) means "a child THIS call
//     created" is not actually a reliable fact: a concurrent SECOND
//     accept(go) racing the SAME card (a Go button double-click, or an
//     accept racing a direct "go" click) can observe the SAME
//     already-running child via that dedup, and if THIS call's own
//     transition Tx is the one that loses the race (the other caller's
//     already committed), aborting here kills the OTHER caller's
//     successfully-dispatched, actually-running child with no error ever
//     surfacing to the caller whose accept genuinely won. That failure mode
//     — silently killing someone else's already-succeeded work — is worse
//     than the orphan this compensation was trying to prevent, so recording
//     the child ids for a human to act on replaces auto-aborting them. See
//     recordDispatchError's own doc comment for the same reasoning in
//     code-adjacent form.
//
// This is also the concrete improvement over v1's known gap (workflow_action.go's
// old "ready" chain): a v1 Dispatch failure only ever reached slog.Error, with
// the "ready" action having already committed and the caller already getting
// an HTTP 200. accept(go) failing now ALWAYS surfaces as a synchronous error
// to the caller, with a dispatch_error action in the audit trail either way.
//
// viaAccept distinguishes the two callers (PR #987 review round 2, MEDIUM
// N2): workflow_action.go's direct `req.Type=="go"` early-redirect passes
// false, applyAnswered's accept(go) deferred call (suggestion_accept.go)
// passes true. When true, the suggestion this call is fulfilling was ALREADY
// recorded as accepted by applyAnswered's own "answered" action (committed in
// a separate, earlier Tx, before this function ever runs) — so stripping it
// here must NOT also emit a "suggestion_discarded" audit action the way a
// genuinely superseded (different-verb) suggestion would under LOW 10: doing
// so recorded the accepted suggestion as if it had been thrown away instead,
// on the single most common accept path in the whole feature. See
// recordAndStripSuggestionIfPresent's own doc comment for the discard-and-
// record behavior this deliberately bypasses when viaAccept is true.
func (s *TaskWorkflowService) acceptGo(ctx context.Context, taskID string, viaAccept bool) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "accept(go): Transactor not configured"}
	}

	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, statusErrorForGetTaskErr(err)
	}
	if task.Status != orchestrator.TaskStatusParked {
		return nil, &StatusError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("accept(go): cannot dispatch task in status %q (must be parked)", task.Status),
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
			// scenarios, and 逆輸入2's "working covers 手動対応中 too" means a
			// childless accept(go) is still meaningful (bare manual work).
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
			s.recordDispatchError(taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusConflict, Message: cerr.Error()}
		}
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
			s.recordDispatchError(taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusConflict, Message: cerr.Error()}
		}
		if s.TaskCreator == nil {
			cerr := fmt.Errorf("accept(go): TaskCreator not configured")
			s.recordDispatchError(taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
		}
		var instructions json.RawMessage
		if children[i].Spec.Instruction != "" {
			marshaled, mErr := json.Marshal([]orchestrator.Instruction{{Message: children[i].Spec.Instruction}})
			if mErr != nil {
				cerr := fmt.Errorf("accept(go): marshal instruction: %w", mErr)
				s.recordDispatchError(taskID, task.Status, cerr)
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
			s.recordDispatchError(taskID, task.Status, cerr)
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: cerr.Error()}
		}
		if childTask.Status == orchestrator.TaskStatusPending {
			cerr := fmt.Errorf("accept(go): child task %q (%s) was created but failed to auto-start (still pending)", children[i].ID, childTask.ID)
			s.recordDispatchError(taskID, task.Status, cerr)
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
	// concurrentTransitionWon distinguishes step 4's ONE specific failure mode
	// (PR #987 review round 2, LOW N4) from every other Tx failure below: this
	// exact branch means another accept(go)/direct "go" already committed the
	// parked->working transition before THIS Tx opened. In that specific
	// case, newlyDispatched must NOT be reported as orphaned_child_task_ids
	// (see the txErr handling below) — CreateTask's own (ref, parent_id)
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
		if fresh.Status != orchestrator.TaskStatusParked {
			concurrentTransitionWon = true
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"accept(go): task status changed to %q before the parked->working transition could commit",
					fresh.Status),
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
		if err := tx.CreateAction(action); err != nil {
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
			if err := tx.CreateAction(childAction); err != nil {
				return fmt.Errorf("accept(go): record child_dispatched for %q: %w", c.ID, err)
			}
		}
		if childrenChanged {
			tt, gErr := tx.GetTaskTriage(taskID)
			if gErr != nil {
				return fmt.Errorf("accept(go): get task_triage for children update: %w", gErr)
			}
			newDetail, sErr := orchestrator.SetDetailChildren(tt.Detail, children)
			if sErr != nil {
				return sErr
			}
			tt.Detail = newDetail
			if err := tx.UpsertTaskTriage(tt); err != nil {
				return err
			}
		}
		// design doc §3.1/§3.3, refined by PR #987 review LOW 10 and round 2's
		// MEDIUM N2: discard any existing suggestion ONLY here, now that the
		// "go" transition has actually committed successfully within this same
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
		} else if err := recordAndStripSuggestionIfPresent(tx, taskID, action); err != nil {
			return err
		}
		newTask = applied
		return nil
	})
	if txErr != nil {
		// Step 4 (PR #987 review, BLOCKER 4 — this PR no longer compensates
		// by aborting): every child above was ALREADY successfully created
		// and auto-started before this Tx failed, so their task ids are
		// folded into the dispatch_error payload for a human to inspect —
		// see recordDispatchError's own doc comment for why aborting them
		// here was removed rather than kept (a concurrent accept(go) racing
		// the same card can, via CreateTask's get-or-create dedup, end up
		// "owning" a child a DIFFERENT caller's Tx actually committed
		// successfully; best-effort-aborting it then kills real in-flight
		// work with no error surfaced to the caller whose accept actually
		// won the race). The card is left parked (this Tx never committed),
		// suggestion intact — same as before.
		//
		// PR #987 review round 2, LOW N4: when concurrentTransitionWon is
		// true specifically, newlyDispatched is deliberately NOT folded into
		// orphaned_child_task_ids. This losing Tx cannot tell whether these
		// task ids are genuinely THIS call's unclaimed children, or the
		// OTHER (winning) accept's own legitimately-running children that
		// CreateTask's get-or-create dedup happened to hand back to this
		// call too — reporting them here would hand an operator a false
		// lead to hand-abort real, successful work. Every OTHER Tx failure
		// (a genuine write error, not "someone else already won the race")
		// still reports them, since those really are this call's own.
		var orphanedChildTaskIDs []string
		if !concurrentTransitionWon {
			for _, c := range newlyDispatched {
				if c.TaskRef != "" {
					orphanedChildTaskIDs = append(orphanedChildTaskIDs, c.TaskRef)
				}
			}
		}
		s.recordDispatchError(taskID, task.Status, txErr, orphanedChildTaskIDs...)
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
