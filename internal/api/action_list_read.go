package api

// docs/plans/ingestion-identity.md PR-3 (B-3): BoidOpActionList's service
// method. Scoping (project vs workspace vs "no filter" → AllowedProjectIDs)
// is resolved by the CALLER (internal/server/boid_executor.go), mirroring
// ListTriage's own division of labor — this method just forwards an
// already-scoped orchestrator.ActionListFilter to the store and shapes the
// result.

import (
	"errors"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ActionListResult is ListActions' return shape: the matching actions plus
// the cursor a follow-up call should pass as filter.Since.
type ActionListResult struct {
	Actions    []*orchestrator.Action `json:"actions"`
	NextCursor string                 `json:"next_cursor"`
}

// ListActions returns the actions matching filter (already scoped by the
// caller) plus the next cursor. See orchestrator.ListActionsSince's own doc
// comment for the pagination contract.
func (s *TaskWorkflowService) ListActions(filter orchestrator.ActionListFilter) (*ActionListResult, error) {
	if s.Actions == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "action list: ActionListStore not configured"}
	}
	actions, nextCursor, err := s.Actions.ListActionsSince(filter)
	if err != nil {
		if errors.Is(err, orchestrator.ErrActionListUnscoped) {
			return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "action list: " + err.Error()}
	}
	if actions == nil {
		actions = []*orchestrator.Action{}
	}
	return &ActionListResult{Actions: actions, NextCursor: nextCursor}, nil
}
