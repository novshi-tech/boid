package orchestrator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// TaskTriage is the cross-project-issue-triage Phase 1 sidecar row for a
// triage task (docs/plans/cross-project-issue-triage.md 実測c). It is
// deliberately kept out of Task / TaskService / TaskStore: Task is the API
// DTO (marshaled to JSON as-is, no conversion layer), so a column added
// there auto-exposes in every API/CLI/Web response. TaskTriage lives only
// where triage-specific code explicitly reads/writes it.
//
// Urgency and WakeAt are real columns because they are queue predicates
// (queue の決定論的評価 節, 決定12) — everything else that doesn't drive a SQL
// WHERE clause (summary/source/content_ref/children/suggestion/observed)
// lives in the Detail JSON blob instead of growing more columns.
//
// There is deliberately no ParkedFrom column: the origin of a park (triaged
// vs ready) is derived from the actions log (see ParkedFrom below), not
// duplicated into a second write path that could go stale (決定13: event
// 追記を正、state は導出).
type TaskTriage struct {
	TaskID     string          `json:"task_id"`
	Kind       string          `json:"kind,omitempty"`    // signal|issue|theme
	Urgency    string          `json:"urgency,omitempty"` // now|today|week|someday
	WakeAt     *time.Time      `json:"wake_at,omitempty"` // 日時wake条件。nil = 無し
	WakeTaskID string          `json:"wake_task_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
}

// UpsertTaskTriage inserts or updates the sidecar row for TaskID.
func UpsertTaskTriage(dbtx db.DBTX, tt *TaskTriage) error {
	if tt.TaskID == "" {
		return fmt.Errorf("upsert task_triage: task_id is required")
	}
	detail := tt.Detail
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	_, err := dbtx.Exec(
		`INSERT INTO task_triage (task_id, kind, urgency, wake_at, wake_task_id, detail)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   kind = excluded.kind,
		   urgency = excluded.urgency,
		   wake_at = excluded.wake_at,
		   wake_task_id = excluded.wake_task_id,
		   detail = excluded.detail`,
		tt.TaskID, tt.Kind, tt.Urgency, nullableTime(tt.WakeAt), tt.WakeTaskID, string(detail),
	)
	if err != nil {
		return fmt.Errorf("upsert task_triage: %w", err)
	}
	return nil
}

// GetTaskTriage retrieves the sidecar row for taskID. Returns an error
// wrapping sql.ErrNoRows when no row exists.
func GetTaskTriage(dbtx db.DBTX, taskID string) (*TaskTriage, error) {
	row := dbtx.QueryRow(
		`SELECT task_id, kind, urgency, wake_at, wake_task_id, detail FROM task_triage WHERE task_id = ?`,
		taskID,
	)
	var tt TaskTriage
	var wakeAt sql.NullTime
	var detail string
	if err := row.Scan(&tt.TaskID, &tt.Kind, &tt.Urgency, &wakeAt, &tt.WakeTaskID, &detail); err != nil {
		return nil, fmt.Errorf("get task_triage %q: %w", taskID, err)
	}
	if wakeAt.Valid {
		t := wakeAt.Time
		tt.WakeAt = &t
	}
	tt.Detail = json.RawMessage(detail)
	return &tt, nil
}

// DeleteTaskTriage removes the sidecar row for taskID. Deleting the parent
// task row already cascades this via ON DELETE CASCADE (see
// 0035_add_task_triage.sql) — this is for explicit cleanup when a task's
// triage sidecar needs to be dropped without deleting the task itself.
func DeleteTaskTriage(dbtx db.DBTX, taskID string) error {
	if _, err := dbtx.Exec(`DELETE FROM task_triage WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("delete task_triage %q: %w", taskID, err)
	}
	return nil
}

// ParkedFrom derives the status a task was parked from (triaged or ready) by
// looking at the most recent "park" action recorded for it. This is the
// origin TaskWorkflowService.Wake uses to choose wake_triaged vs wake_ready
// — see machine.go's NewMachine doc comment for why this is not a stored
// column.
func ParkedFrom(dbtx db.DBTX, taskID string) (TaskStatus, error) {
	row := dbtx.QueryRow(
		`SELECT from_status FROM actions WHERE task_id = ? AND type = 'park' ORDER BY created_at DESC LIMIT 1`,
		taskID,
	)
	var from string
	if err := row.Scan(&from); err != nil {
		return "", fmt.Errorf("parked_from %q: %w", taskID, err)
	}
	status, ok := ParseTaskStatus(from)
	if !ok {
		return "", fmt.Errorf("parked_from %q: unrecognized from_status %q on park action", taskID, from)
	}
	return status, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// Child status vocabulary for task_triage.detail.children (逆輸入1:
// task_spec/task_ref/open_items were unified into this single list). Status
// progresses open → specced → dispatched → closed; PR-2 only acts on
// "specced" (task-ifying it into "dispatched" — see TaskWorkflowService.Dispatch).
const (
	TaskTriageChildStatusOpen       = "open"
	TaskTriageChildStatusSpecced    = "specced"
	TaskTriageChildStatusDispatched = "dispatched"
	TaskTriageChildStatusClosed     = "closed"
)

// TaskTriageChildSpec is the execution recipe a child needs before it can be
// task-ified (docs/plans/cross-project-issue-triage.md データモデル節). Project
// is assumed to already be a resolved boid project ID by the time a child
// reaches "specced" — PR-2 does not do project-ref fuzzy resolution (決定5's
// routing confirmation is a 整形セッション / Phase 2 concern); an unresolvable
// Project surfaces as a plain "project not found" error from CreateTask.
type TaskTriageChildSpec struct {
	Project     string `json:"project"`
	Behavior    string `json:"behavior,omitempty"`
	Instruction string `json:"instruction,omitempty"`
}

// TaskTriageChild is one entry of task_triage.detail.children.
type TaskTriageChild struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// Status is one of the TaskTriageChildStatus* constants above.
	Status string `json:"status"`
	// Spec is required from "specced" onward (this + the child's Title is
	// everything task-ification needs).
	Spec *TaskTriageChildSpec `json:"spec,omitempty"`
	// TaskRef is set once the child has been task-ified ("dispatched" onward):
	// the id of the real boid task TaskWorkflowService.Dispatch created for it.
	TaskRef string `json:"task_ref,omitempty"`
}

// DetailChildren unmarshals the "children" array out of a task_triage.Detail
// blob. Returns (nil, nil) for an empty/absent detail or a detail with no
// "children" key — both mean "no children yet", not an error.
func DetailChildren(detail json.RawMessage) ([]TaskTriageChild, error) {
	if len(detail) == 0 || string(detail) == "null" {
		return nil, nil
	}
	var wrapper struct {
		Children []TaskTriageChild `json:"children"`
	}
	if err := json.Unmarshal(detail, &wrapper); err != nil {
		return nil, fmt.Errorf("parse task_triage detail children: %w", err)
	}
	return wrapper.Children, nil
}

// SetDetailChildren returns a new detail blob with the "children" key
// replaced by children, leaving every other top-level key of detail
// untouched (summary/source/content_ref/suggestion/observed etc — 逆輸入1/3).
// detail's shape is a schema-light JSON blob by design (実測c), so this
// round-trips through a map rather than a fixed Go struct to avoid silently
// dropping fields PR-2's code doesn't know about.
func SetDetailChildren(detail json.RawMessage, children []TaskTriageChild) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(detail) > 0 && string(detail) != "null" {
		if err := json.Unmarshal(detail, &m); err != nil {
			return nil, fmt.Errorf("parse task_triage detail: %w", err)
		}
	}
	childrenJSON, err := json.Marshal(children)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail children: %w", err)
	}
	m["children"] = childrenJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail: %w", err)
	}
	return out, nil
}
