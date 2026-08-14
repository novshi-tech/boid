package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TaskTriageView is the read-side projection of a triage task
// (docs/plans/cross-project-issue-triage.md Phase 1 PR-5a).
//
// 決定14 makes the daemon the SOLE source of truth for a triage task's state:
// khi retires its own decisions log and fold, keeping only claims (source
// dialect) and the note body. That is only viable if the workspace side can
// read the state back, and read it COMPLETELY — including the fields that are
// derived rather than stored. Before PR-5a there was no read surface at all:
// the sidecar was reachable only from inside the daemon (Web UI's queue_next
// enrichment), and `boid task show` returns orchestrator.Task's own columns,
// which deliberately exclude everything triage-specific (see TaskTriageStore's
// doc comment).
//
// The view unions three sources:
//
//   - the task row (status/project/title/description) — state itself lives
//     here, not in the sidecar
//   - the task_triage columns (kind/urgency/wake_at/wake_task_id)
//   - ParkedFrom, DERIVED from the actions log and stored nowhere (決定13:
//     event 追記を正、state は導出). Omitting it would be the exact gap the
//     PR was asked to close — "action から導出するフィールドが取れないのは
//     余計おかしい".
//
// Detail is passed through verbatim as an opaque blob. The daemon does not
// interpret its keys (逆輸入3's boundary: channel-specific knowledge stays on
// the workspace side), so neither does this projection.
type TaskTriageView struct {
	TaskID    string                  `json:"task_id"`
	ProjectID string                  `json:"project_id"`
	Title     string                  `json:"title,omitempty"`
	Status    orchestrator.TaskStatus `json:"status"`
	// Description is the task row's body. It rides along with Title for the
	// same reason Title does — a caller rendering the card needs both — but it
	// was missed in PR-5a. The cost of that omission is concrete: khi's
	// project_card.py renders the note body FROM this view, so without it every
	// sweep pays one extra `boid task show --field description` round trip per
	// card just to diff what it is about to write (2026-08-14).
	Description string `json:"description,omitempty"`

	Kind       string     `json:"kind,omitempty"`
	Urgency    string     `json:"urgency,omitempty"`
	WakeAt     *time.Time `json:"wake_at,omitempty"`
	WakeTaskID string     `json:"wake_task_id,omitempty"`

	// ParkedFrom is only populated for a parked task: it answers "which side
	// will this wake back to (triaged or ready)". Deliberately left empty for
	// every other status rather than reporting the most recent historical park
	// — a stale origin on a task that is no longer parked would be actively
	// misleading to a caller deciding whether Go has already been given.
	ParkedFrom orchestrator.TaskStatus `json:"parked_from,omitempty"`

	Detail json.RawMessage `json:"detail,omitempty"`
}

// GetTriage returns the read-side projection of one triage task.
//
// A missing TASK is an error (404). A missing SIDECAR ROW is not: the task's
// status is the single most important piece of state, and a triage task can
// legitimately exist without a sidecar row (a row created before PR-5a's
// CreateTask seeding, or one whose first side-effect action has not landed
// yet). Reporting 404 for those would tell the caller "this task does not
// exist" about a task that plainly does — the same fail-open posture
// Dispatch already takes when it treats a missing row as "no children".
func (s *TaskWorkflowService) GetTriage(taskID string) (*TaskTriageView, error) {
	if s.Tasks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "triage: TaskStore not configured"}
	}
	task, err := s.Tasks.GetTask(taskID)
	if err != nil {
		return nil, statusErrorForGetTaskErr(err)
	}
	var tt *orchestrator.TaskTriage
	if s.TaskTriage != nil {
		loaded, ttErr := s.TaskTriage.GetTaskTriage(taskID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("triage: get task_triage %q: %v", taskID, ttErr)}
			}
		} else {
			tt = loaded
		}
	}
	return s.triageViewFor(task, tt), nil
}

// ListTriage returns the projections of every TRIAGE task matching filter.
//
// The discriminator is "has a task_triage row" rather than "has a
// triage-shaped status". Status alone cannot decide it: `done` is shared with
// ordinary tasks (a done triage task and a done executor task are
// indistinguishable by status — the same limitation that forces PR-5b's
// reopen guard into the service layer), and a meta project holds plenty of
// ordinary tasks (the ingest/sweep executor tasks) that must not appear in a
// triage listing. CreateTask seeds the row for every pre-execution task
// (task_create.go), which is what makes this predicate reliable from birth.
//
// A task whose sidecar lookup fails for a reason OTHER than "no row" is
// skipped rather than aborting the whole listing: one malformed row must not
// make the entire queue unreadable (same posture as SweepWake).
func (s *TaskWorkflowService) ListTriage(filter orchestrator.TaskFilter) ([]*TaskTriageView, error) {
	if s.Tasks == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "triage: TaskStore not configured"}
	}
	// Default floor (Opus review round 3): an unset status would run
	// ListTasks(TaskFilter{}) — every task row ever created — and then one
	// sidecar point query per row, almost all of which return ErrNoRows and
	// are discarded. "triage" is pre-execution ∪ working, i.e. exactly the
	// live cards a caller means by "list the triage tasks"; done/dropped
	// remain reachable by naming the status explicitly. WebHandler.TaskList
	// defaults to status=open for the same reason.
	if filter.Status == "" {
		filter.Status = "triage"
	}
	tasks, err := s.Tasks.ListTasks(filter)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "triage list: " + err.Error()}
	}
	views := make([]*TaskTriageView, 0, len(tasks))
	if s.TaskTriage == nil {
		return views, nil
	}
	for _, t := range tasks {
		tt, ttErr := s.TaskTriage.GetTaskTriage(t.ID)
		if ttErr != nil {
			if !errors.Is(ttErr, sql.ErrNoRows) {
				// An unreadable row makes the card vanish from the queue read
				// surface with nothing anywhere saying why — log it rather than
				// dropping it in silence (Opus review round 2; SweepWake, the
				// precedent this posture comes from, logs the same case).
				slog.Warn("triage list: get task_triage failed; card omitted from the listing",
					"task_id", t.ID, "error", ttErr)
			}
			// sql.ErrNoRows: simply not a triage task.
			continue
		}
		views = append(views, s.triageViewFor(t, tt))
	}
	return views, nil
}

// triageViewFor builds the projection for an already-loaded task and its
// already-loaded sidecar row (tt may be nil: "task exists, no sidecar yet").
func (s *TaskWorkflowService) triageViewFor(task *orchestrator.Task, tt *orchestrator.TaskTriage) *TaskTriageView {
	view := &TaskTriageView{
		TaskID:      task.ID,
		ProjectID:   task.ProjectID,
		Title:       task.Title,
		Description: task.Description,
		Status:      task.Status,
	}
	if tt == nil {
		return view
	}
	view.Kind = tt.Kind
	view.Urgency = tt.Urgency
	view.WakeAt = tt.WakeAt
	view.WakeTaskID = tt.WakeTaskID
	view.Detail = tt.Detail

	if task.Status == orchestrator.TaskStatusParked {
		// A parked task with no resolvable park action is a real anomaly but
		// not a reason to fail the whole read — leave ParkedFrom empty and let
		// the caller see the rest of the state (Wake itself surfaces the same
		// condition as a 409 when it actually matters).
		if from, pErr := s.TaskTriage.ParkedFrom(task.ID); pErr == nil {
			view.ParkedFrom = from
		}
	}
	return view
}
