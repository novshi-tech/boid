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

// SeedTaskTriage creates an empty sidecar row for taskID if one does not
// already exist, and does nothing at all if one does. This is the "assert
// this task IS a triage task" primitive (Phase 1 PR-5a): unlike
// UpsertTaskTriage it can never overwrite an existing row's columns, so a
// caller that only means to mark membership cannot destroy state.
//
// Single-statement by design (Opus review round 2, Low): a get-then-upsert
// from the caller has a TOCTOU window — an attrs_set for the same card
// committing between the read and the write would be blanked by the seed.
// ON CONFLICT DO NOTHING closes it in the database rather than relying on
// the window being narrow.
func SeedTaskTriage(dbtx db.DBTX, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("seed task_triage: task_id is required")
	}
	if _, err := dbtx.Exec(
		`INSERT INTO task_triage (task_id) VALUES (?) ON CONFLICT(task_id) DO NOTHING`,
		taskID,
	); err != nil {
		return fmt.Errorf("seed task_triage: %w", err)
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

// parseDetailMap round-trips a task_triage.Detail blob through a
// map[string]json.RawMessage so callers can replace a single top-level key
// (children, attrs, ...) without disturbing keys they don't know about
// (実測c: detail is a schema-light JSON blob by design).
func parseDetailMap(detail json.RawMessage) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(detail) > 0 && string(detail) != "null" {
		if err := json.Unmarshal(detail, &m); err != nil {
			return nil, fmt.Errorf("parse task_triage detail: %w", err)
		}
	}
	return m, nil
}

// SetDetailChildren returns a new detail blob with the "children" key
// replaced by children, leaving every other top-level key of detail
// untouched (summary/source/content_ref/suggestion/observed etc — 逆輸入1/3).
// detail's shape is a schema-light JSON blob by design (実測c), so this
// round-trips through a map rather than a fixed Go struct to avoid silently
// dropping fields PR-2's code doesn't know about.
func SetDetailChildren(detail json.RawMessage, children []TaskTriageChild) (json.RawMessage, error) {
	m, err := parseDetailMap(detail)
	if err != nil {
		return nil, err
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

// StripDetailAttrs removes keys from detail's top-level "attrs" object,
// leaving every other top-level key untouched. It backs the promotion of
// urgency/kind out of the opaque blob and into their real columns (Phase 1
// PR-5a): those two are queue predicates, and keeping a second copy in the
// blob would let the value the queue SQL reads drift from the value a
// workspace-side reader sees. Migration 0040 converges existing rows; this
// keeps the invariant self-healing for any row that somehow still carries a
// stale copy, without depending on the migration having run.
func StripDetailAttrs(detail json.RawMessage, keys ...string) (json.RawMessage, error) {
	m, err := parseDetailMap(detail)
	if err != nil {
		return nil, err
	}
	raw, ok := m["attrs"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return detail, nil
	}
	attrs := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &attrs); err != nil {
		return nil, fmt.Errorf("parse task_triage detail attrs: %w", err)
	}
	changed := false
	for _, k := range keys {
		if _, present := attrs[k]; present {
			delete(attrs, k)
			changed = true
		}
	}
	if !changed {
		return detail, nil
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail attrs: %w", err)
	}
	m["attrs"] = attrsJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail: %w", err)
	}
	return out, nil
}

// FoldDetailAttrs merges patch into detail's top-level "attrs" object,
// last-write-wins per key, leaving every other top-level key (children,
// summary, ...) untouched. This backs the "attrs_set" action vocabulary
// entry (docs/plans/cross-project-issue-triage.md PR-4 設計メモ 論点4/6).
//
// Deliberately a PURE fold with zero policy: it does not know or care what
// keys mean (urgency, summary, ...) and does not enforce monotonicity or any
// other invariant on a key's value across writes. That kind of policy (e.g.
// "urgency only ever increases") is khi's responsibility on the evaluate
// side, per 論点6's closing note — encoding it here would be a boundary
// violation the same way `applyParkSideEffect` does not decide whether a
// wake_at is "reasonable".
func FoldDetailAttrs(detail json.RawMessage, patch map[string]json.RawMessage) (json.RawMessage, error) {
	m, err := parseDetailMap(detail)
	if err != nil {
		return nil, err
	}
	attrs := map[string]json.RawMessage{}
	if raw, ok := m["attrs"]; ok && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &attrs); err != nil {
			return nil, fmt.Errorf("parse task_triage detail attrs: %w", err)
		}
	}
	for k, v := range patch {
		attrs[k] = v
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail attrs: %w", err)
	}
	m["attrs"] = attrsJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal task_triage detail: %w", err)
	}
	return out, nil
}

// AddDetailChild appends child to detail's "children" list, defaulting
// Status to TaskTriageChildStatusOpen when unset. Idempotent by ID: if a
// child with the same ID already exists, detail is returned unchanged (a
// resend after a crash between khi recording the push and receiving the ack
// must not duplicate the child entry — same dedup posture as the Ref-based
// task-create get-or-create, 論点7).
func AddDetailChild(detail json.RawMessage, child TaskTriageChild) (json.RawMessage, error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		if c.ID == child.ID {
			return detail, nil
		}
	}
	if child.Status == "" {
		child.Status = TaskTriageChildStatusOpen
	}
	children = append(children, child)
	return SetDetailChildren(detail, children)
}

// SpecDetailChild finds the child with the given id and sets its Spec/Title,
// advancing its status to TaskTriageChildStatusSpecced. Returns an error if
// no child with that id exists — unlike AddDetailChild this is not
// create-or-update: "specced" only ever applies to a child khi already
// announced via child_added (or one seeded directly in the child's own
// detail at task-create time).
//
// If the child has already progressed past "specced" — i.e. it is
// dispatched or closed — this is an idempotent no-op (detail returned
// unchanged, found=true, no error): those two statuses are daemon-recorded
// MECHANICAL FACTS (child_dispatched/child_closed, 論点9), and a khi resend
// of child_specced after an uncertain ack (crash/timeout before receiving
// the response) must never regress one of them back to "specced" — that
// would let a stale duplicate action send erase "this child already ran and
// finished" (codex review Major fix). Re-specc'ing a child that is still
// "open" or already "specced" (re-editing the spec before Go) both apply
// normally.
func SpecDetailChild(detail json.RawMessage, id string, spec TaskTriageChildSpec, title string) (json.RawMessage, error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range children {
		if children[i].ID == id {
			found = true
			switch children[i].Status {
			case TaskTriageChildStatusDispatched, TaskTriageChildStatusClosed:
				// Idempotent no-op: do not regress a daemon-recorded
				// mechanical fact.
				return detail, nil
			}
			specCopy := spec
			children[i].Spec = &specCopy
			if title != "" {
				children[i].Title = title
			}
			children[i].Status = TaskTriageChildStatusSpecced
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("child %q not found in task_triage detail", id)
	}
	return SetDetailChildren(detail, children)
}

// MarkDetailChildClosed marks the child entry whose TaskRef equals
// childTaskID as closed. This is the daemon's own self-record of the
// "child_closed" vocabulary entry (論点9) — khi never calls this; the
// TaskWorkflowService hooks it directly into the child task's own
// done/aborted finalization path. changed=false means no matching,
// not-already-closed child was found (already closed by an earlier call, or
// this task was never linked via TaskRef) — callers use this to skip
// appending a duplicate action.
func MarkDetailChildClosed(detail json.RawMessage, childTaskID string) (out json.RawMessage, changed bool, err error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return nil, false, err
	}
	for i := range children {
		if children[i].TaskRef == childTaskID && children[i].Status != TaskTriageChildStatusClosed {
			children[i].Status = TaskTriageChildStatusClosed
			newDetail, serr := SetDetailChildren(detail, children)
			if serr != nil {
				return nil, false, serr
			}
			return newDetail, true, nil
		}
	}
	return detail, false, nil
}
