package orchestrator

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

// CardAttrs is the card-only field set. Every function below reads/writes
// the tasks row addressed by task_id, filtered to type='card', rather than a
// separate table.
//
// Urgency and WakeAt are real columns because they are queue predicates —
// everything else that doesn't drive a SQL WHERE clause
// (summary/source/content_ref/children/suggestion/observed) lives in the
// Detail JSON blob instead of growing more columns.
//
// There is deliberately no ParkedFrom column: the origin of a park (triaged
// vs ready vs working) is derived from the actions log (see ParkedFrom
// below), not duplicated into a second write path that could go stale.
type CardAttrs struct {
	TaskID     string     `json:"task_id"`
	Kind       string     `json:"kind,omitempty"`    // signal|issue|theme
	Urgency    string     `json:"urgency,omitempty"` // now|today|week|someday
	WakeAt     *time.Time `json:"wake_at,omitempty"` // 日時wake条件。nil = 無し
	WakeTaskID string     `json:"wake_task_id,omitempty"`
	// SuggestionVerb is the promoted suggestion column: one of
	// go/working/park/drop/done/reopen (orchestrator.IsCardTransitionAction),
	// or "" when the card carries no current suggestion. Mirrors Kind/Urgency
	// — a real column, not opaque blob-only data — because it backs the
	// `/api/cards` read surface (CardView.SuggestionVerb) and the daemon's
	// own notifySuggestionArrived/updated_at-bump change-detection (both key
	// off comparing this column's old vs. new value). Unlike Kind/Urgency the
	// full suggestion (reason/params) stays in Detail's JSON blob too; only
	// the verb is duplicated into this column.
	SuggestionVerb string          `json:"suggestion_verb,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

// UpsertTaskTriage writes CardAttrs' columns onto the tasks row identified
// by tt.TaskID. Despite the name (kept for call-site compatibility), this is
// only ever an UPDATE — every card row already has these columns from the
// moment CreateTask makes it type='card'. The affected-row check turns
// "taskID doesn't exist or isn't a card" into a clear error instead of a
// silent no-op (a bare UPDATE with no matching WHERE row returns no error
// from database/sql).
func UpsertTaskTriage(dbtx db.DBTX, tt *CardAttrs) error {
	if tt.TaskID == "" {
		return fmt.Errorf("upsert card attrs: task_id is required")
	}
	detail := tt.Detail
	if len(detail) == 0 {
		detail = json.RawMessage("{}")
	}
	res, err := dbtx.Exec(
		`UPDATE tasks SET kind = ?, urgency = ?, wake_at = ?, wake_task_id = ?, suggestion_verb = ?, detail = ?
		 WHERE id = ? AND type = 'card'`,
		tt.Kind, tt.Urgency, nullableTime(tt.WakeAt), tt.WakeTaskID, tt.SuggestionVerb, string(detail), tt.TaskID,
	)
	if err != nil {
		return fmt.Errorf("upsert card attrs: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("upsert card attrs %q: %w", tt.TaskID, ErrTaskNotFound)
	}
	return nil
}

// GetTaskTriage retrieves the CardAttrs columns for taskID. Returns an error
// wrapping sql.ErrNoRows when the task does not exist or is not a card.
func GetTaskTriage(dbtx db.DBTX, taskID string) (*CardAttrs, error) {
	row := dbtx.QueryRow(
		`SELECT id, kind, urgency, wake_at, wake_task_id, suggestion_verb, detail FROM tasks WHERE id = ? AND type = 'card'`,
		taskID,
	)
	var tt CardAttrs
	var wakeAt sql.NullTime
	var kind, urgency, wakeTaskID, suggestionVerb, detail sql.NullString
	if err := row.Scan(&tt.TaskID, &kind, &urgency, &wakeAt, &wakeTaskID, &suggestionVerb, &detail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get card attrs %q: %w", taskID, sql.ErrNoRows)
		}
		return nil, fmt.Errorf("get card attrs %q: %w", taskID, err)
	}
	tt.Kind = kind.String
	tt.Urgency = urgency.String
	tt.WakeTaskID = wakeTaskID.String
	tt.SuggestionVerb = suggestionVerb.String
	if wakeAt.Valid {
		t := wakeAt.Time
		tt.WakeAt = &t
	}
	tt.Detail = json.RawMessage(detail.String)
	return &tt, nil
}

// taskTriageInClauseChunkSize bounds how many task IDs go into a single
// `WHERE task_id IN (...)` query in ListTaskTriageByTaskIDs — a wide safety
// margin under sqlite's compiled-in SQLITE_MAX_VARIABLE_NUMBER (32766).
const taskTriageInClauseChunkSize = 500

// ListTaskTriageByTaskIDs batch-fetches task_triage sidecar rows for a set
// of task IDs in O(chunks) queries instead of O(len(taskIDs)). A taskID with
// no sidecar row is simply absent from the returned map — the same "missing
// means no enrichment" contract GetTaskTriage's sql.ErrNoRows expresses,
// just as map absence instead of a per-call error.
//
// Best-effort across BOTH axes, so one malformed row cannot sink the whole
// call:
//   - a single row's Scan error is skipped, not fatal — every other row in
//     that chunk, and every other chunk, still gets processed
//   - a whole chunk's Query failing (or a mid-stream rows.Err()) does not
//     discard rows already gathered from earlier chunks; remaining chunks
//     still run
//
// The first error encountered (if any) is still returned alongside
// whatever partial map was gathered — the map itself, not the error, is the
// thing to check for "did I get anything useful".
func ListTaskTriageByTaskIDs(dbtx db.DBTX, taskIDs []string) (map[string]*CardAttrs, error) {
	out := map[string]*CardAttrs{}
	var firstErr error
	noteErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for start := 0; start < len(taskIDs); start += taskTriageInClauseChunkSize {
		end := start + taskTriageInClauseChunkSize
		if end > len(taskIDs) {
			end = len(taskIDs)
		}
		chunk := taskIDs[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		func() {
			rows, err := dbtx.Query(
				`SELECT id, kind, urgency, wake_at, wake_task_id, suggestion_verb, detail FROM tasks WHERE type = 'card' AND id IN (`+placeholders+`)`,
				args...,
			)
			if err != nil {
				noteErr(fmt.Errorf("list task_triage by task_ids: query: %w", err))
				return
			}
			defer rows.Close()
			for rows.Next() {
				var tt CardAttrs
				var wakeAt sql.NullTime
				var kind, urgency, wakeTaskID, suggestionVerb, detail sql.NullString
				if err := rows.Scan(&tt.TaskID, &kind, &urgency, &wakeAt, &wakeTaskID, &suggestionVerb, &detail); err != nil {
					noteErr(fmt.Errorf("list task_triage by task_ids: scan: %w", err))
					continue
				}
				tt.Kind = kind.String
				tt.Urgency = urgency.String
				tt.WakeTaskID = wakeTaskID.String
				tt.SuggestionVerb = suggestionVerb.String
				if wakeAt.Valid {
					t := wakeAt.Time
					tt.WakeAt = &t
				}
				tt.Detail = json.RawMessage(detail.String)
				out[tt.TaskID] = &tt
			}
			if err := rows.Err(); err != nil {
				noteErr(fmt.Errorf("list task_triage by task_ids: %w", err))
			}
		}()
	}
	return out, firstErr
}

// DeleteTaskTriage resets a card's CardAttrs columns back to their empty
// defaults without deleting the task row itself. Has no production caller —
// kept for interface/test-double compatibility.
func DeleteTaskTriage(dbtx db.DBTX, taskID string) error {
	res, err := dbtx.Exec(
		`UPDATE tasks SET kind = '', urgency = '', wake_at = NULL, wake_task_id = '', suggestion_verb = '', detail = '{}'
		 WHERE id = ? AND type = 'card'`,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("delete task_triage %q: %w", taskID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete task_triage %q: %w", taskID, ErrTaskNotFound)
	}
	return nil
}

// ParkedFrom derives the status a task was parked from (triaged, ready, or
// working) by looking at the most recent "park" action recorded for it. This
// is the origin TaskWorkflowService.Wake uses to choose wake_triaged vs
// wake_ready vs wake_working — see machine.go's NewMachine doc comment for
// why this is not a stored column.
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

// Child status vocabulary for task_triage.detail.children. Status
// progresses open → specced → dispatched → closed (task-ifying "specced"
// into "dispatched" — see TaskWorkflowService.Dispatch).
const (
	TaskTriageChildStatusOpen       = "open"
	TaskTriageChildStatusSpecced    = "specced"
	TaskTriageChildStatusDispatched = "dispatched"
	TaskTriageChildStatusClosed     = "closed"
)

// TaskTriageChildSpec is the execution recipe a child needs before it can be
// task-ified. Project is assumed to already be a resolved boid project ID by
// the time a child reaches "specced" — there is no project-ref fuzzy
// resolution here; an unresolvable Project surfaces as a plain "project not
// found" error from CreateTask.
type TaskTriageChildSpec struct {
	Project  string `json:"project"`
	Behavior string `json:"behavior,omitempty"`
	// Description is the child task's Web UI-visible description — the
	// narrative/background content (source context, what to do, links).
	// Kept separate from Instruction so the agent-facing instructions
	// message can stay a short, reusable procedure while the human-facing
	// context lives where the Web UI actually renders it.
	Description string `json:"description,omitempty"`
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

// DetailOpenSlotChildID returns the id of the first child in detail's
// "children" list whose status is open or specced — a child not yet
// task-ified — or "" if none exists. This is the JSON-only half of a
// card's single-work-slot check; a dispatched child is tracked by its own
// live task row instead (see workflow_card.go's cardSlotOccupied and
// task_create.go's cardChildSlotConflict, the two callers that combine this
// with a real-row check), not by this blob, so it is deliberately excluded
// here.
//
// A card that already violates the invariant (legacy data predating this
// check) is not an error here: this returns whichever occupant it finds
// first, in list order. Enumerating every offending card for cleanup is a
// separate diagnostic (cmd's card multi-child report), not this function's
// job.
func DetailOpenSlotChildID(detail json.RawMessage) (string, error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return "", err
	}
	for _, c := range children {
		if c.Status == TaskTriageChildStatusOpen || c.Status == TaskTriageChildStatusSpecced {
			return c.ID, nil
		}
	}
	return "", nil
}

// CountUnresolvedChildren returns the total number of children currently
// contending for a card's single work slot: every JSON child not yet
// task-ified (open/specced) plus openChildCount — the live (pending/
// executing/awaiting) execution task rows under the card (a dispatched
// JSON child's own row, or one created via a direct-CreateTask bypass with
// no JSON entry at all; see task.OpenChildCount).
//
// Unlike DetailOpenSlotChildID (which the write-time gates use to ask only
// "is there ANY occupant"), this answers "how many", so a card that
// accumulated more than one before this invariant existed can be found and
// listed (the `boid task diagnose-cards` CLI).
func CountUnresolvedChildren(detail json.RawMessage, openChildCount int) (int, error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return 0, err
	}
	n := openChildCount
	for _, c := range children {
		if c.Status == TaskTriageChildStatusOpen || c.Status == TaskTriageChildStatusSpecced {
			n++
		}
	}
	return n, nil
}

// Suggestion is task_triage.detail.suggestion — the triage agent's single
// recommendation, derived from primary sources. Verb uses the same
// vocabulary as nose's own response words: go/shape/manual/park/drop/wake.
type Suggestion struct {
	Verb   string `json:"verb,omitempty"`
	Action string `json:"action,omitempty"`
	Reason string `json:"reason,omitempty"`
	Basis  string `json:"basis,omitempty"`
}

// DetailSuggestion extracts task_triage.detail.suggestion from a detail
// blob, preferring the top-level key and falling back to
// detail.attrs.suggestion (the path actually exercised today, since
// attrs_set — the only write path for it — folds into detail.attrs, not the
// top level; top-level is still checked first in case a future/alternate
// writer places it there directly). Both paths are kept regardless — see
// decodeSuggestion's doc comment for why an empty top-level object must not
// win over a real one in attrs.
//
// Each candidate (top-level, then attrs) is decoded independently via its
// own json.Unmarshal call: a malformed or wrong-shaped value in one
// location must not blank out a well-formed value in the other.
//
// Returns the zero Suggestion and false on any absence/parse failure — never
// errors, so a missing/malformed suggestion blob never sinks the row it
// lives on.
func DetailSuggestion(detail json.RawMessage) (Suggestion, bool) {
	if len(detail) == 0 || string(detail) == "null" {
		return Suggestion{}, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(detail, &top); err != nil {
		return Suggestion{}, false
	}
	if s, ok := decodeSuggestion(top["suggestion"]); ok {
		return s, true
	}
	var attrs map[string]json.RawMessage
	if err := json.Unmarshal(top["attrs"], &attrs); err == nil {
		if s, ok := decodeSuggestion(attrs["suggestion"]); ok {
			return s, true
		}
	}
	return Suggestion{}, false
}

// decodeSuggestion decodes a single candidate "suggestion" value. Returns
// (zero, false) for an absent/null/malformed value rather than erroring —
// see DetailSuggestion's doc comment for why each candidate must fail
// independently of the other.
//
// A value that decodes to the zero Suggestion (e.g. `{}`, or an object with
// only unrecognized field names) also returns false: DetailSuggestion tries
// top-level first and only falls back to attrs on (zero, false), so an empty
// top-level object would otherwise "win" over a real suggestion in attrs and
// the row would render with no suggestion at all.
func decodeSuggestion(raw json.RawMessage) (Suggestion, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return Suggestion{}, false
	}
	var s Suggestion
	if err := json.Unmarshal(raw, &s); err != nil {
		return Suggestion{}, false
	}
	if s == (Suggestion{}) {
		return Suggestion{}, false
	}
	return s, true
}

// DetailSuggestionRaw returns the raw JSON bytes of task_triage.detail's
// current suggestion (same top-level-then-attrs precedence as
// DetailSuggestion above), for a caller that needs MORE than the fixed
// Suggestion struct's four fields expose — e.g. accept(verb)'s
// params.wake_at/wake_task_id, a write-side addition Suggestion itself does
// not carry. Reuses decodeSuggestion purely as the "is this candidate
// non-empty and well-formed" check, so the two functions can never disagree
// about WHICH candidate wins.
func DetailSuggestionRaw(detail json.RawMessage) (json.RawMessage, bool) {
	if len(detail) == 0 || string(detail) == "null" {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(detail, &top); err != nil {
		return nil, false
	}
	if _, ok := decodeSuggestion(top["suggestion"]); ok {
		return top["suggestion"], true
	}
	var attrs map[string]json.RawMessage
	if err := json.Unmarshal(top["attrs"], &attrs); err == nil {
		if _, ok := decodeSuggestion(attrs["suggestion"]); ok {
			return attrs["suggestion"], true
		}
	}
	return nil, false
}

// parseDetailMap round-trips a task_triage.Detail blob through a
// map[string]json.RawMessage so callers can replace a single top-level key
// (children, attrs, ...) without disturbing keys they don't know about —
// detail is a schema-light JSON blob by design.
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
// untouched (summary/source/content_ref/suggestion/observed etc). detail's
// shape is a schema-light JSON blob by design, so this round-trips through
// a map rather than a fixed Go struct to avoid silently dropping fields this
// code doesn't know about.
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
// urgency/kind out of the opaque blob and into their real columns: those two
// are queue predicates, and keeping a second copy in the blob would let the
// value the queue SQL reads drift from the value a workspace-side reader
// sees. This keeps the invariant self-healing for any row that somehow
// still carries a stale copy.
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
// entry.
//
// Deliberately a PURE fold with zero policy: it does not know or care what
// keys mean (urgency, summary, ...) and does not enforce monotonicity or any
// other invariant on a key's value across writes. That kind of policy (e.g.
// "urgency only ever increases") is khi's responsibility on the evaluate
// side — encoding it here would be a boundary violation the same way
// `applyParkSideEffect` does not decide whether a wake_at is "reasonable".
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
// must not duplicate the child entry).
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
// mechanical facts, and a khi resend of child_specced after an uncertain
// ack must never regress one of them back to "specced". Re-specc'ing a
// child that is still "open" or already "specced" (re-editing the spec
// before Go) both apply normally.
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
// "child_closed" vocabulary entry — khi never calls this; the
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

// DropDetailChild closes the child with the given id because khi decided not
// to pursue it — a wrong project baked into the spec, a duplicate of another
// child, or a stray left behind while the subagent was exploring the write
// CLI. Idempotent: an already-closed child returns changed=false so a resend
// after a lost ack does not append a duplicate action.
//
// This is the counterpart MarkDetailChildClosed cannot be: that one matches
// on TaskRef, which only exists once a child has been task-ified, so a child
// still sitting at "open" or "specced" can never reach "closed" through it.
// Without this function such a child is unclosable, and since ShouldAutoDone
// requires EVERY child closed, one stray entry keeps its whole card from
// ever finishing.
//
// A "dispatched" child is refused. That status is the daemon's own
// mechanical fact and its task is still running: closing it here would let
// ShouldAutoDone fire while real work is in flight. Withdrawing live work is
// an abort of the child's task, not a triage-detail edit.
func DropDetailChild(detail json.RawMessage, id string) (out json.RawMessage, changed bool, err error) {
	children, err := DetailChildren(detail)
	if err != nil {
		return nil, false, err
	}
	for i := range children {
		if children[i].ID != id {
			continue
		}
		switch children[i].Status {
		case TaskTriageChildStatusClosed:
			return detail, false, nil
		case TaskTriageChildStatusDispatched:
			return nil, false, fmt.Errorf("child %q is dispatched; abort its task instead of dropping the entry", id)
		}
		children[i].Status = TaskTriageChildStatusClosed
		newDetail, serr := SetDetailChildren(detail, children)
		if serr != nil {
			return nil, false, serr
		}
		return newDetail, true, nil
	}
	return nil, false, fmt.Errorf("child %q not found in task_triage detail", id)
}
