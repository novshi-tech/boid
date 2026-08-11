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

// Wake is the single user/PR-2/3-facing verb for reviving a parked task. It
// resolves the origin (triaged vs ready) via ParkedFrom — derived from the
// actions log, not a stored column — and applies the matching internal
// action (wake_triaged/wake_ready, rejected if sent directly through the
// public ApplyAction path — see StateMachine.IsManualAction, used by
// ApplyAction's guard in workflow_action.go). This exists specifically so no
// caller can wake a task to the wrong status: getting triaged vs ready wrong
// would silently promote a task past Go without nose's judgment (決定9/逆輸入2).
//
// Unlike ApplyAction's general shape (task read, then a separate
// WithinTx write), Wake re-reads the task AND resolves ParkedFrom from
// inside the SAME transaction as the write. Splitting those into a pre-tx
// read and a later write left a window where a concurrent park→wake→park
// cycle on the same task could change the actions-log origin between the
// read and the write, so a wake in flight could apply against a stale
// origin (codex review round 1, Major). Reading everything transactionally
// closes that window: whichever origin is committed at write time is the
// one that decides wake_triaged vs wake_ready, with no gap to race into.
func (s *TaskWorkflowService) Wake(ctx context.Context, taskID string) (*ActionApplication, error) {
	if s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "wake: Transactor not configured"}
	}

	sm := orchestrator.DefaultMachine()
	var newTask *orchestrator.Task
	var action *orchestrator.Action

	if err := s.Tx.WithinTx(func(tx TxStore) error {
		fresh, err := tx.GetTask(taskID)
		if err != nil {
			return &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		if fresh.Status != orchestrator.TaskStatusParked {
			return &StatusError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("cannot wake task in status %q (must be parked)", fresh.Status),
			}
		}
		from, err := tx.ParkedFrom(taskID)
		if err != nil {
			return &StatusError{Code: http.StatusConflict, Message: "wake: cannot resolve park origin: " + err.Error()}
		}
		var resolvedType string
		switch from {
		case orchestrator.TaskStatusTriaged:
			resolvedType = "wake_triaged"
		case orchestrator.TaskStatusReady:
			resolvedType = "wake_ready"
		default:
			return &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("wake: unexpected park origin %q", from)}
		}

		action = &orchestrator.Action{TaskID: taskID, Type: resolvedType}
		newTask, err = sm.Apply(fresh, action)
		if err != nil {
			return &StatusError{Code: http.StatusConflict, Message: err.Error()}
		}
		action.FromStatus = fresh.Status
		action.ToStatus = newTask.Status

		if err := tx.UpdateTask(newTask); err != nil {
			return err
		}
		return tx.CreateAction(action)
	}); err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) {
			return nil, statusErr
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
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

	return &ActionApplication{Task: newTask, Action: action}, nil
}
