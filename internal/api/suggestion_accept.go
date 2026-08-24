package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// suggestionParams is attrs.suggestion.params' shape (docs/plans/
// suggestion-as-state-transition-impl.md §3): only meaningful for verb=="park"
// today (park needs a wake condition), but left open-shaped (both fields
// optional, no verb-specific struct) rather than a closed union, since a
// future verb might need its own params without a schema migration.
type suggestionParams struct {
	WakeAt     string `json:"wake_at,omitempty"`
	WakeTaskID string `json:"wake_task_id,omitempty"`
}

// suggestionAttr is attrs.suggestion's shape: {verb, reason, params?}. This
// mirrors orchestrator.Suggestion (task_triage.go, the READ side —
// DetailSuggestion) but is declared separately here because Params is new in
// this PR and orchestrator.Suggestion is a stable read-side type this PR does
// not otherwise touch (avoiding a cross-package doc-comment/JSON-tag
// entanglement for a field only the WRITE-side validator below needs).
type suggestionAttr struct {
	Verb   string           `json:"verb"`
	Reason string           `json:"reason,omitempty"`
	Params suggestionParams `json:"params,omitempty"`
}

// validateSuggestionAttr validates attrs_set's "suggestion" key BEFORE it
// folds into task_triage.detail.attrs (docs/plans/
// suggestion-as-state-transition-impl.md §3: "verb ∈ {go, working, park,
// drop, done, reopen} を daemon が検証する"). This does NOT cross the
// workspace-vocabulary boundary J-7 protects elsewhere in this package
// (verb/basis on the "answered" action are recorded but never
// cross-checked): the six verbs here are boid's OWN state-machine
// vocabulary (orchestrator.IsCardTransitionAction), not a workspace-defined
// one — see design doc §3.1's own framing, "boid 自身の状態遷移に限る".
//
// A JSON null value is accepted unconditionally (clearing the suggestion via
// attrs_set — mirrors parsePromotedAttr's own null-clears-the-column
// convention for urgency/kind, workflow_triage.go). Everything else must be
// a JSON object with a verb from the closed set; params.wake_at, if present,
// must parse as RFC3339 (the same format applyParkSideEffect's own
// parseParkPayload already requires for a direct park action's wake_at).
//
// Returns the validated verb (""  for a null/clearing payload) alongside the
// error — PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1):
// this is now the single parse/validate pass parseAttrsSetPayload's
// "suggestion" case reuses to populate attrsSetPatch.Verb/HasVerb, the value
// applyAttrsSetSideEffect promotes into task_triage.suggestion_verb. Before
// this PR the only caller discarded the verb after validating it and
// parseAttrsSetPayload just folded the raw value into the opaque blob — a
// second parse of the same JSON to extract the verb for the column would
// have been genuinely redundant (and risked drifting from this function's
// own validation if the two ever disagreed on what counts as "known").
func validateSuggestionAttr(raw json.RawMessage) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var s suggestionAttr
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload: suggestion: " + err.Error()}
	}
	if !orchestrator.IsCardTransitionAction(s.Verb) {
		return "", &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid attrs_set payload: suggestion.verb %q is not a known card transition (allowed: go, working, park, drop, done, reopen)", s.Verb),
		}
	}
	if s.Params.WakeAt != "" {
		if _, err := time.Parse(time.RFC3339, s.Params.WakeAt); err != nil {
			return "", &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload: suggestion.params.wake_at: " + err.Error()}
		}
	}
	return s.Verb, nil
}

// applyParkSideEffectFromSuggestion applies accept(park)'s wake condition
// from the ACCEPTED SUGGESTION's own params — not from the "answered"
// action's payload, which carries only {answer, verb, basis} (see
// answeredPayload). This reuses applyParkSideEffect's established
// read-modify-write (workflow_triage.go) via the same *parkPayload shape
// parseParkPayload produces for a direct park action, so a suggested park
// and a directly-clicked park converge on one writer.
func applyParkSideEffectFromSuggestion(tx TxStore, taskID string, params suggestionParams) error {
	p := &parkPayload{WakeAt: params.WakeAt, WakeTaskID: params.WakeTaskID}
	return applyParkSideEffect(tx, taskID, p)
}

// noSuggestionToAcceptErr is applyAnswered's signal for "answer:accept was
// sent but there is no current suggestion (or its verb is unrecognized) to
// apply" — a 409, not a 500: the caller asked to accept something that no
// longer exists (a stale Web UI page, a race with a fresh khi re-suggest),
// not a daemon malfunction.
func noSuggestionToAcceptErr(detail string) *StatusError {
	return &StatusError{Code: http.StatusConflict, Message: "answered: no suggestion to accept" + detail}
}

// applyAnswered is the "answered" action's full implementation (docs/plans/
// suggestion-as-state-transition-impl.md §3.3, design doc §3.1): records the
// answered action, and — on accept — applies the suggestion's own verb as a
// real card transition BEFORE dropping the suggestion (§3.1: "遷移を適用して
// から suggestion を消す" — an accept that fails must not discard the
// suggestion it failed to act on). Reject is unchanged from v1: the
// suggestion is dropped unconditionally, no transition applied.
//
// Bypasses ApplyAction's generic action pipeline entirely (workflow_action.go
// calls straight into this for req.Type=="answered", before any of the
// meta-hydration / payload-merge / skipTaskUpdate machinery built for the
// OTHER card actions) — same posture as Wake/Dispatch always took for their
// own dedicated multi-step flows. "answered" only ever applies to a card (a
// suggestion only exists on a task_triage sidecar row), so NewCardMachine is
// unambiguous here without a machineFor lookup, exactly like Wake/Dispatch
// established.
//
// 穴11 push-down defense: accept (not reject) is a card TRANSITION in
// disguise — it is what actually flips the card's status, via whichever verb
// the suggestion names — so it gets the SAME actor==human requirement
// orchestrator.IsCardTransitionAction's six verbs get directly in
// workflow_action.go. Without this check here, khi could bypass that guard
// entirely by pushing `answered{accept}` instead of the verb itself (neither
// "answered" nor its non-transitioning ToStatus=="" rule are in
// IsCardTransitionAction's set, so the generic guard alone would let it
// through). reject is NOT gated: it can never cause a transition either way.
func (s *TaskWorkflowService) applyAnswered(ctx context.Context, taskID string, payload json.RawMessage) (*ActionApplication, error) {
	parsed, perr := parseAnsweredPayload(payload)
	if perr != nil {
		return nil, perr
	}
	if parsed.Answer == answeredAnswerAccept && orchestrator.ActorFromContext(ctx) != orchestrator.ActorHuman {
		return nil, &StatusError{
			Code:    http.StatusForbidden,
			Message: "answered{accept} is a card-lifecycle transition and may only be applied by a human (Web UI / CLI)",
		}
	}
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "answered: Transactor not configured"}
	}

	sm := orchestrator.NewCardMachine()
	action := &orchestrator.Action{TaskID: taskID, Type: "answered", Payload: payload, Actor: orchestrator.ActorFromContext(ctx)}
	var newTask *orchestrator.Task
	acceptGoRequested := false

	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, ferr := tx.GetTask(taskID)
		if ferr != nil {
			return statusErrorForGetTaskErr(ferr)
		}
		applied, aerr := sm.Apply(fresh, action)
		if aerr != nil {
			return &StatusError{Code: http.StatusConflict, Message: aerr.Error()}
		}
		action.FromStatus = fresh.Status
		action.ToStatus = applied.Status // non-transitioning: equals fresh.Status
		if err := tx.CreateAction(action); err != nil {
			return err
		}

		if parsed.Answer != answeredAnswerAccept {
			newTask = applied
			return applyAnsweredSideEffect(tx, taskID)
		}

		tt, ttErr := tx.GetTaskTriage(taskID)
		if ttErr != nil {
			if errors.Is(ttErr, sql.ErrNoRows) {
				return noSuggestionToAcceptErr(" (no task_triage row)")
			}
			return fmt.Errorf("answered: get task_triage: %w", ttErr)
		}
		raw, ok := orchestrator.DetailSuggestionRaw(tt.Detail)
		if !ok {
			return noSuggestionToAcceptErr("")
		}
		var sugg suggestionAttr
		if err := json.Unmarshal(raw, &sugg); err != nil {
			return fmt.Errorf("answered: parse suggestion: %w", err)
		}
		if !orchestrator.IsCardTransitionAction(sugg.Verb) {
			// Defensive: validateSuggestionAttr already rejects an unknown
			// verb at attrs_set time, so this guards only against a row
			// written before that validation existed, or written directly
			// to the DB.
			return noSuggestionToAcceptErr(fmt.Sprintf(" (verb %q is not a known card transition)", sugg.Verb))
		}

		if sugg.Verb == "go" {
			// go needs non-transactional child creation BEFORE its own
			// transition commits (acceptGo's own doc comment,
			// workflow_triage.go) — cannot run nested inside THIS Tx.
			// Deferred to after this Tx commits; newTask stays at the
			// pre-transition (parked) snapshot until acceptGo's own result
			// overwrites it below.
			acceptGoRequested = true
			newTask = applied
			return nil
		}

		verbAction := &orchestrator.Action{TaskID: taskID, Type: sugg.Verb, Actor: orchestrator.ActorFromContext(ctx)}
		verbApplied, vaerr := sm.Apply(applied, verbAction)
		if vaerr != nil {
			// PR-3: replace sm.Apply's raw `no transition for action %q from
			// status %q` (vaerr.Error(), still logged nowhere — this message IS
			// the only trace) with one that also says what WOULD have worked,
			// via orchestrator.StateMachine.AvailableActionsHint's own doc
			// comment (single source shared with the Web UI's inapplicable-
			// suggestion notice — review LOW 4).
			return &StatusError{
				Code: http.StatusConflict,
				Message: fmt.Sprintf(
					"suggestion (verb=%s) cannot be applied from status=%s; %s",
					sugg.Verb, applied.Status, sm.AvailableActionsHint(applied.Status),
				),
			}
		}
		verbAction.FromStatus = applied.Status
		verbAction.ToStatus = verbApplied.Status
		if err := tx.UpdateTask(verbApplied); err != nil {
			return err
		}
		if err := tx.CreateAction(verbAction); err != nil {
			return err
		}
		switch sugg.Verb {
		case "park":
			if err := applyParkSideEffectFromSuggestion(tx, taskID, sugg.Params); err != nil {
				return err
			}
		case "drop":
			// docs/plans/ingestion-identity.md PR-1 (B-1), I-6: same identity
			// release a direct drop action already gets (workflow_action.go).
			if err := tx.UnlinkAllForTask(taskID); err != nil {
				return err
			}
		}
		// design doc §3.1: strip the suggestion ONLY here, now that the verb's
		// transition has actually committed successfully within this same Tx.
		if err := applyAnsweredSideEffect(tx, taskID); err != nil {
			return err
		}
		newTask = verbApplied
		return nil
	})
	if txErr != nil {
		var statusErr *StatusError
		if errors.As(txErr, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: txErr.Error()}
	}

	if s.Hub != nil {
		s.Hub.Broadcast(taskID, TaskEvent{
			Kind: "action",
			Payload: map[string]any{
				"action_id":  action.ID,
				"new_status": string(action.ToStatus),
			},
		})
	}

	if acceptGoRequested {
		// viaAccept=true (PR #987 review round 2, MEDIUM N2): the suggestion
		// this call fulfills was already recorded as accepted by the
		// "answered" action just committed above — acceptGo must strip it
		// without ALSO recording a misleading "suggestion_discarded" audit
		// entry for the very suggestion that was just accepted, not thrown
		// away. See acceptGo's own doc comment.
		applied, goErr := s.acceptGo(ctx, taskID, true)
		if goErr != nil {
			// The "answered" action itself already committed above (決定13:
			// the accept-attempt is a real audit fact regardless of whether
			// the mechanical follow-through succeeded) — but the caller must
			// still see this as a failed request: the card never actually
			// reached working, and acceptGo's own failure path already left
			// the suggestion in place and recorded dispatch_error.
			return nil, goErr
		}
		newTask = applied.Task
	}

	return &ActionApplication{Task: newTask, Action: action}, nil
}
