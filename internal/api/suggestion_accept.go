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

// suggestionParams is attrs.suggestion.params' shape: only meaningful for
// verb=="park" today (park needs a wake condition), but left open-shaped
// rather than a closed union, since a future verb might need its own
// params without a schema migration.
type suggestionParams struct {
	WakeAt     string `json:"wake_at,omitempty"`
	WakeTaskID string `json:"wake_task_id,omitempty"`
}

// suggestionAttr is attrs.suggestion's shape: {verb, reason, params?}. This
// mirrors orchestrator.Suggestion (card.go's DetailSuggestion, the read
// side) but is declared separately since Params is only needed by this
// write-side validator.
type suggestionAttr struct {
	Verb   string           `json:"verb"`
	Reason string           `json:"reason,omitempty"`
	Params suggestionParams `json:"params,omitempty"`
}

// validateSuggestionAttr validates attrs_set's "suggestion" key before it
// folds into task_triage.detail.attrs. The six verbs (go, start, park,
// drop, complete, reopen) are boid's own state-machine vocabulary
// (orchestrator.IsCardTransitionAction) — not a workspace-defined one, so
// this does not cross the workspace-vocabulary boundary the rest of this
// package protects (verb/basis on the "answered" action are recorded but
// never cross-checked).
//
// A JSON null value is accepted unconditionally (clearing the suggestion
// via attrs_set — mirrors parsePromotedAttr's null-clears-the-column
// convention for urgency/kind). Everything else must be a JSON object
// with a verb from the closed set (after normalization, see below);
// params.wake_at, if present, must parse as RFC3339 (the same format a
// direct park action's wake_at requires).
//
// A verb still spelled the retired way ("working"/"done" — a short compat
// window for an old write CLI) is normalized to its current name
// (orchestrator.NormalizeCardVerb) before the closed-set check runs, and
// the returned normalized bytes carry the current spelling too — so a stale
// spelling is never persisted into detail.attrs.suggestion.verb by a fresh
// write. reason/params are preserved byte-for-byte; only the "verb" key is
// ever replaced.
//
// Returns the normalized raw suggestion bytes and the validated verb ("",
// nil for a null/clearing payload). This is the single parse/validate pass
// parseAttrsSetPayload's "suggestion" case reuses to populate both
// attrsSetPatch.Attrs["suggestion"] and Verb/HasVerb — the value
// applyAttrsSetSideEffect promotes into task_triage.suggestion_verb.
func validateSuggestionAttr(raw json.RawMessage) (json.RawMessage, string, error) {
	if string(raw) == "null" {
		return raw, "", nil
	}
	var s suggestionAttr
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, "", &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload: suggestion: " + err.Error()}
	}
	verb := orchestrator.NormalizeCardVerb(s.Verb)
	if !orchestrator.IsCardTransitionAction(verb) {
		return nil, "", &StatusError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid attrs_set payload: suggestion.verb %q is not a known card transition (allowed: go, start, park, drop, complete, reopen)", s.Verb),
		}
	}
	if s.Params.WakeAt != "" {
		if _, err := time.Parse(time.RFC3339, s.Params.WakeAt); err != nil {
			return nil, "", &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload: suggestion.params.wake_at: " + err.Error()}
		}
	}
	normalized := raw
	if verb != s.Verb {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, "", &StatusError{Code: http.StatusBadRequest, Message: "invalid attrs_set payload: suggestion: " + err.Error()}
		}
		verbJSON, err := json.Marshal(verb)
		if err != nil {
			return nil, "", &StatusError{Code: http.StatusInternalServerError, Message: "marshal normalized suggestion verb: " + err.Error()}
		}
		m["verb"] = verbJSON
		out, err := json.Marshal(m)
		if err != nil {
			return nil, "", &StatusError{Code: http.StatusInternalServerError, Message: "marshal normalized suggestion: " + err.Error()}
		}
		normalized = out
	}
	return normalized, verb, nil
}

// applyParkSideEffectFromSuggestion applies accept(park)'s wake condition
// from the ACCEPTED SUGGESTION's own params — not from the "answered"
// action's payload, which carries only {answer, verb, basis} (see
// answeredPayload). This reuses applyParkSideEffect's established
// read-modify-write (workflow_card.go) via the same *parkPayload shape
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

// applyAnswered is the "answered" action's full implementation: records the
// answered action, and — on accept — applies the suggestion's own verb as
// a real card transition BEFORE dropping the suggestion (an accept that
// fails must not discard the suggestion it failed to act on). Reject is
// unchanged: the suggestion is dropped unconditionally, no transition
// applied.
//
// Bypasses ApplyAction's generic action pipeline entirely (workflow_action.go
// calls straight into this for req.Type=="answered") — same posture as
// Wake/Dispatch for their own dedicated multi-step flows. "answered" only
// ever applies to a card, so NewCardMachine is unambiguous here without a
// machineFor lookup.
//
// accept (not reject) is a card transition in disguise — it is what
// actually flips the card's status, via whichever verb the suggestion
// names — so it gets the same actor==human requirement
// orchestrator.IsCardTransitionAction's six verbs get directly in
// workflow_action.go. Without this check, khi could bypass that guard by
// pushing `answered{accept}` instead of the verb itself. reject is NOT
// gated: it can never cause a transition either way.
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
		if err := tx.CreateAction(ctx, action); err != nil {
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
		// A stored suggestion still spelled the retired way ("working"/
		// "done" — a row from before the data migration/write-side
		// normalization existed) reads as its current name here too, not
		// just at write time.
		sugg.Verb = orchestrator.NormalizeCardVerb(sugg.Verb)
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
			// workflow_card.go) — cannot run nested inside THIS Tx.
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
			// Replace sm.Apply's raw "no transition for action %q from status
			// %q" with one that also says what would have worked, via
			// orchestrator.StateMachine.AvailableActionsHint (the single
			// source the Web UI's inapplicable-suggestion notice also uses).
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
		if err := tx.CreateAction(ctx, verbAction); err != nil {
			return err
		}
		switch sugg.Verb {
		case "park":
			if err := applyParkSideEffectFromSuggestion(tx, taskID, sugg.Params); err != nil {
				return err
			}
		case "drop":
			// Same identity release a direct drop action already gets
			// (workflow_action.go).
			if err := tx.UnlinkAllForTask(taskID); err != nil {
				return err
			}
		}
		// Strip the suggestion only here, now that the verb's transition has
		// actually committed successfully within this same Tx.
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
		// viaAccept=true: the suggestion this call fulfills was already
		// recorded as accepted by the "answered" action just committed
		// above — acceptGo must strip it without ALSO recording a
		// misleading "suggestion_discarded" audit entry for the suggestion
		// that was just accepted. See acceptGo's own doc comment.
		applied, goErr := s.acceptGo(ctx, taskID, true)
		if goErr != nil {
			// The "answered" action itself already committed above (a real
			// audit fact regardless of whether the mechanical follow-through
			// succeeded) — but the caller must still see this as a failed
			// request: the card never actually reached working, and
			// acceptGo's own failure path already left the suggestion in
			// place and recorded dispatch_error.
			return nil, goErr
		}
		newTask = applied.Task
	}

	return &ActionApplication{Task: newTask, Action: action}, nil
}
