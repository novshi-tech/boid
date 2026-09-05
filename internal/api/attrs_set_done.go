package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// resolveAttrsSetDoneTransition special-cases attrs_set against a "done"
// task: if the task carries a task_triage row (a card), this is treated as
// a routine, non-transitioning attrs_set (like on any preExecutionStatuses
// task) instead of the rejection sm.Apply would otherwise produce.
//
// This is a service-layer guard, not a machine.go rule change, because
// machine.go's preExecutionStatuses FromStatus list is shared across five
// action verbs (attrs_set/child_added/child_specced/noted/answered) —
// widening it to include "done" would open the door for all five, not just
// attrs_set. Background: docs/plans/ingestion-identity.md (I-5b).
func resolveAttrsSetDoneTransition(sm *orchestrator.StateMachine, task *orchestrator.Task, action *orchestrator.Action, getTriage func(string) (*orchestrator.CardAttrs, error)) (*orchestrator.Task, *StatusError) {
	if action.Type == "attrs_set" && task.Status == orchestrator.TaskStatusDone && getTriage != nil {
		_, err := getTriage(task.ID)
		switch {
		case err == nil:
			// A task_triage row exists: non-transitioning, like any other
			// preExecutionStatuses attrs_set. CloneTaskShallow (not a bare
			// copy) avoids aliasing noop.Card back into task.Card.
			return orchestrator.CloneTaskShallow(task), nil
		case errors.Is(err, sql.ErrNoRows):
			// No row: an ordinary done task — falls through to sm.Apply's rejection below.
		default:
			// A genuine lookup failure must not be silently reinterpreted as "no row, reject".
			return nil, &StatusError{Code: http.StatusInternalServerError, Message: "attrs_set: check task_triage: " + err.Error()}
		}
	}
	applied, aerr := sm.Apply(task, action)
	if aerr != nil {
		return nil, &StatusError{Code: http.StatusConflict, Message: aerr.Error()}
	}
	return applied, nil
}

// logAttrsSetOnDoneTriage logs (at Debug) every attrs_set that lands on a
// done triage task via the guard above. Debug rather than Warn: this is now
// the ordinary, expected way a done card receives new information, not a
// signal something needs attention — this is purely an ops trace.
func logAttrsSetOnDoneTriage(taskTriage CardStore, taskID string) {
	if taskTriage == nil {
		return
	}
	tt, err := taskTriage.GetTaskTriage(taskID)
	if err != nil {
		slog.Debug("attrs_set landed on a done triage task, but re-reading task_triage failed", "task_id", taskID, "error", err)
		return
	}
	slog.Debug("attrs_set landed on a done triage task",
		"task_id", taskID, "source_closed", orchestrator.SourceClosed(tt.Detail))
}
