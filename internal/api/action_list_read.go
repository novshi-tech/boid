package api

// ListActions backs BoidOpActionList. Scoping (project vs workspace vs no
// filter) is resolved by the caller (internal/server/boid_executor.go);
// this just forwards an already-scoped filter to the store.

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
