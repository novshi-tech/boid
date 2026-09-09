package orchestrator

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
)

const (
	OperationResultAccepted = "accepted"
	OperationResultStarted  = "started"
	OperationResultRejected = "rejected"
	OperationResultUnknown  = "unknown"

	OperationReasonRequestAccepted                  = "request_accepted"
	OperationReasonExecutionStarted                 = "execution_started"
	OperationReasonSlotOccupied                     = "slot_occupied"
	OperationReasonNotAvailable                     = "not_available"
	OperationReasonInvalidRequest                   = "invalid_request"
	OperationReasonNotFound                         = "not_found"
	OperationReasonConflict                         = "conflict"
	OperationReasonInternalError                    = "internal_error"
	OperationReasonNoReadyWork                      = "no_ready_work"
	OperationReasonRequestFailed                    = "request_failed"
	OperationReasonTargetStarted                    = "target_started"
	OperationReasonOutcomePending                   = "outcome_pending"
	OperationReasonSuggestionAcceptedLaunchRejected = "suggestion_accepted_launch_rejected"
	OperationReasonSuggestionAcceptedLaunchUnknown  = "suggestion_accepted_launch_unknown"
)

// OperationResult records the server-observed outcome of one human UI operation.
type OperationResult struct {
	ID                       string
	TaskID                   string
	OperationType            string
	OperationLabel           string
	Result                   string
	ReasonCode               string
	TargetTaskID             string
	TargetSessionID          string
	TargetTaskUnavailable    bool
	TargetSessionUnavailable bool
	TargetRequestID          string
	CurrentPhase             string
	Notice                   string
	CreatedAt                time.Time
}

type OperationResultStore struct{ db db.DBTX }

func NewOperationResultStore(dbtx db.DBTX) *OperationResultStore {
	return &OperationResultStore{db: dbtx}
}

func (s *OperationResultStore) CreateOperationResult(r *OperationResult) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO operation_results
		(id, task_id, operation_type, operation_label, result, reason_code, target_task_id, target_session_id, target_request_id, current_phase, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.TaskID, r.OperationType, r.OperationLabel, r.Result, r.ReasonCode,
		r.TargetTaskID, r.TargetSessionID, r.TargetRequestID, r.CurrentPhase, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("create operation result: %w", err)
	}
	return nil
}

// UpdateOperationResult finalizes an initial unknown receipt exactly once.
func (s *OperationResultStore) UpdateOperationResult(r *OperationResult) error {
	res, err := s.db.Exec(`UPDATE operation_results SET
		operation_label = ?, result = ?, reason_code = ?, target_task_id = ?, target_session_id = ?, target_request_id = ?, current_phase = ?
		WHERE id = ? AND task_id = ? AND result = ? AND reason_code = ?`,
		r.OperationLabel, r.Result, r.ReasonCode, r.TargetTaskID, r.TargetSessionID, r.TargetRequestID, r.CurrentPhase,
		r.ID, r.TaskID, OperationResultUnknown, OperationReasonOutcomePending)
	if err != nil {
		return fmt.Errorf("update operation result: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update operation result rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("update operation result: pending receipt not found")
	}
	return nil
}

// ListOperationResults returns newest first and resolves a request's eventual
// task or session association at read time so revisiting the card gains the link.
func (s *OperationResultStore) ListOperationResults(taskID string, limit int) ([]*OperationResult, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT o.id, o.task_id, o.operation_type, o.operation_label, o.result, o.reason_code,
		CASE WHEN cr.target_kind = 'task' THEN cr.target_id ELSE o.target_task_id END,
		CASE WHEN cr.target_kind = 'session' THEN cr.target_id ELSE o.target_session_id END,
		o.target_request_id, o.created_at,
		CASE WHEN o.result = 'accepted' AND o.reason_code = 'request_accepted' AND cr.status = 'failed' THEN 'request_failed'
		     WHEN o.result = 'accepted' AND o.reason_code = 'request_accepted' AND cr.status IN ('attached','finished') THEN 'target_started' ELSE o.current_phase END,
        NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = CASE WHEN cr.target_kind = 'task' THEN cr.target_id ELSE o.target_task_id END),
        NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id = CASE WHEN cr.target_kind = 'session' THEN cr.target_id ELSE o.target_session_id END)
		FROM operation_results o LEFT JOIN card_requests cr ON cr.id = o.target_request_id
		WHERE o.task_id = ? ORDER BY o.created_at DESC, o.id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("list operation results: %w", err)
	}
	defer rows.Close()
	results := make([]*OperationResult, 0)
	for rows.Next() {
		r := new(OperationResult)
		if err := rows.Scan(&r.ID, &r.TaskID, &r.OperationType, &r.OperationLabel, &r.Result, &r.ReasonCode,
			&r.TargetTaskID, &r.TargetSessionID, &r.TargetRequestID, &r.CreatedAt, &r.CurrentPhase, &r.TargetTaskUnavailable, &r.TargetSessionUnavailable); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// AllOperationResults supplies the complete history to the item-paginated timeline.
func (s *OperationResultStore) AllOperationResults(taskID string) ([]*OperationResult, error) {
	return s.listOperationResults("o.task_id = ?", []any{taskID}, -1)
}

func (s *OperationResultStore) GetOperationResult(taskID, id string) (*OperationResult, error) {
	rows, err := s.listOperationResults(`o.task_id = ? AND o.id = ?`, []any{taskID, id}, 1)
	if err != nil || len(rows) == 0 {
		if err == nil {
			err = sql.ErrNoRows
		}
		return nil, err
	}
	return rows[0], nil
}

func (s *OperationResultStore) listOperationResults(where string, args []any, limit int) ([]*OperationResult, error) {
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT o.id, o.task_id, o.operation_type, o.operation_label, o.result, o.reason_code,
		CASE WHEN cr.target_kind = 'task' THEN cr.target_id ELSE o.target_task_id END,
		CASE WHEN cr.target_kind = 'session' THEN cr.target_id ELSE o.target_session_id END,
		o.target_request_id, o.created_at,
		CASE WHEN o.result = 'accepted' AND o.reason_code = 'request_accepted' AND cr.status = 'failed' THEN 'request_failed'
		     WHEN o.result = 'accepted' AND o.reason_code = 'request_accepted' AND cr.status IN ('attached','finished') THEN 'target_started' ELSE o.current_phase END,
        NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = CASE WHEN cr.target_kind = 'task' THEN cr.target_id ELSE o.target_task_id END),
        NOT EXISTS (SELECT 1 FROM jobs j WHERE j.id = CASE WHEN cr.target_kind = 'session' THEN cr.target_id ELSE o.target_session_id END)
		FROM operation_results o LEFT JOIN card_requests cr ON cr.id = o.target_request_id
		WHERE `+where+` ORDER BY o.created_at DESC, o.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list operation results: %w", err)
	}
	defer rows.Close()
	results := make([]*OperationResult, 0)
	for rows.Next() {
		r := new(OperationResult)
		if err := rows.Scan(&r.ID, &r.TaskID, &r.OperationType, &r.OperationLabel, &r.Result, &r.ReasonCode,
			&r.TargetTaskID, &r.TargetSessionID, &r.TargetRequestID, &r.CreatedAt, &r.CurrentPhase, &r.TargetTaskUnavailable, &r.TargetSessionUnavailable); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func GCOperationResults(dbtx db.DBTX, olderThan time.Duration, dryRun bool) (int64, error) {
	query := `created_at < ?`
	cutoff := time.Now().UTC().Add(-olderThan)
	if dryRun {
		var n int64
		if err := dbtx.QueryRow(`SELECT COUNT(*) FROM operation_results WHERE `+query, cutoff).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	res, err := dbtx.Exec(`DELETE FROM operation_results WHERE `+query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("gc operation results: %w", err)
	}
	return res.RowsAffected()
}
