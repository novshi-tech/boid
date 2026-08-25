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

// CardAttrs is the card-only field set (design doc §3.2's field attribution
// table). Through card-model-cleanup PR-1 it was the cross-project-issue-
// triage Phase 1 sidecar row for a triage task, kept out of Task/TaskService/
// TaskStore because Task was the API DTO (marshaled to JSON as-is) and a
// column added there auto-exposed in every API/CLI/Web response. PR-2
// (docs/plans/card-model-cleanup.md §3.5, migration 0045) folds the
// task_triage table into tasks itself (a card row's kind/urgency/wake_at/
// wake_task_id/suggestion_verb/detail columns) and gives Task a genuinely
// nested `card` JSON key (`Task.Card *CardAttrs`) instead — so this struct
// keeps the exact shape callers already know, but every function below now
// reads/writes the SAME tasks row addressed by task_id, filtered to
// type='card', rather than a separate table.
//
// Urgency and WakeAt are real columns because they are queue predicates
// (queue の決定論的評価 節, 決定12) — everything else that doesn't drive a SQL
// WHERE clause (summary/source/content_ref/children/suggestion/observed)
// lives in the Detail JSON blob instead of growing more columns.
//
// There is deliberately no ParkedFrom column: the origin of a park (triaged
// vs ready vs working) is derived from the actions log (see ParkedFrom
// below), not duplicated into a second write path that could go stale
// (決定13: event 追記を正、state は導出).
type CardAttrs struct {
	TaskID     string     `json:"task_id"`
	Kind       string     `json:"kind,omitempty"`    // signal|issue|theme
	Urgency    string     `json:"urgency,omitempty"` // now|today|week|someday
	WakeAt     *time.Time `json:"wake_at,omitempty"` // 日時wake条件。nil = 無し
	WakeTaskID string     `json:"wake_task_id,omitempty"`
	// SuggestionVerb is the promoted suggestion column (docs/plans/
	// suggestion-as-state-transition-impl.md §4.1, migration 0044): one of
	// go/working/park/drop/done/reopen (orchestrator.IsCardTransitionAction),
	// or "" when the card carries no current suggestion. Mirrors Kind/Urgency
	// — a real column, not opaque blob-only data. Until docs/plans/
	// webui-detail-list-redesign.md PR-4 it was ALSO a live SQL queue
	// predicate (store.go's "queue_next" filter, since removed — §3.6); it
	// stays promoted regardless because it backs the `/api/cards` read
	// surface (CardView.SuggestionVerb) and the daemon's own
	// notifySuggestionArrived/updated_at-bump change-detection (both key off
	// comparing this column's old vs. new value). Unlike Kind/Urgency the
	// full suggestion (reason/params) stays in Detail's JSON blob too; only
	// the verb is duplicated into this column (see applyAttrsSetSideEffect's
	// doc comment, internal/api/workflow_card.go,
	// for why that duplication is intentional and does not drift).
	SuggestionVerb string          `json:"suggestion_verb,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

// UpsertTaskTriage writes CardAttrs' columns onto the tasks row identified
// by tt.TaskID. Despite the name (kept for call-site compatibility across
// PR-2 — see this file's own package doc comment), this is no longer an
// upsert against a separate sidecar table: every card row already has these
// columns (defaulted to their zero value) from the moment CreateTask makes
// it type='card', so there is nothing to insert — only UPDATE. The affected-
// row check turns "taskID doesn't exist or isn't a card" into a clear error
// instead of a silent no-op (a bare UPDATE with no matching WHERE row
// returns no error from database/sql).
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
// wrapping sql.ErrNoRows when the task does not exist or is not a card (the
// same "no row" contract callers wrote against when this queried a separate
// task_triage table — see this file's own package doc comment).
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
// `WHERE task_id IN (...)` query in ListTaskTriageByTaskIDs. sqlite's
// compiled-in SQLITE_MAX_VARIABLE_NUMBER (modernc.org/sqlite v1.34.5,
// vendoring sqlite 3.46.0) is 32766 — this chunk size is a wide safety
// margin under that, not a tuned value: task IDs are short strings (cheap
// to bind), and there is no evidence yet of a workspace with anywhere near
// this many parked/queue_next tasks at once, so staying comfortably clear
// of the placeholder limit matters far more here than minimizing round
// trips.
const taskTriageInClauseChunkSize = 500

// ListTaskTriageByTaskIDs batch-fetches task_triage sidecar rows for a set
// of task IDs in O(chunks) queries instead of O(len(taskIDs)) — the
// Parked/Queue Web UI views used to call GetTaskTriage once per row
// (internal/api/web.go's triageByTaskID), and #task-list polls every 5s
// (tasks.templ's hx-trigger), so that N+1 scaled linearly with poll
// frequency × view size (BD-8 残件1). A taskID with no sidecar row is simply
// absent from the returned map — same "missing means no enrichment"
// contract callers already read off GetTaskTriage's sql.ErrNoRows today,
// just expressed as map absence instead of a per-call error.
//
// Best-effort across BOTH axes, same posture as the old per-task
// GetTaskTriage loop it replaces (Opus review finding, 2026-08-18: an
// earlier version of this function aborted the entire call — discarding
// rows already scanned from prior chunks — on a single row's Scan error,
// which is a real, if narrow, possibility independent of the chunk/query
// succeeding: e.g. a wake_at value written by something other than
// UpsertTaskTriage's own nullableTime path, in a format sql.NullTime can't
// parse. That would have turned exactly one malformed row into "every card
// on the Parked/Queue tab loses its badge" — the class of silent
// whole-view failure 決定9 exists to prevent):
//   - a single row's Scan error is skipped, not fatal — every other row in
//     that chunk, and every other chunk, still gets processed
//   - a whole chunk's Query failing (or a mid-stream rows.Err()) does not
//     discard rows already gathered from earlier chunks; remaining chunks
//     still run
//
// The first error encountered (if any) is still returned alongside
// whatever partial map was gathered, so a caller that wants to log/surface
// it can — but the map itself, not the error, is the thing to check for
// "did I get anything useful" (see triageByTaskID's doc comment,
// internal/api/web.go, for how the actual call site uses this).
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
// defaults without deleting the task row itself. Before card-model-cleanup
// PR-2 (migration 0045) this deleted a separate task_triage sidecar row —
// deleting the parent task row already cascaded that via ON DELETE CASCADE
// (0035_add_task_triage.sql), so the only real use was explicit cleanup of
// the sidecar's CONTENT without touching the task. Now that the content
// lives on the tasks row itself, "delete the sidecar" can only mean
// resetting those columns; the task row (and its type='card') is untouched.
// Has no production caller as of this PR (grep confirms) — kept for
// interface/test-double compatibility, same posture as its predecessor.
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
	Project  string `json:"project"`
	Behavior string `json:"behavior,omitempty"`
	// Description is the child task's Web UI-visible description — the
	// narrative/background content (source context, what to do, links).
	// Kept separate from Instruction so the agent-facing instructions
	// message can stay a short, reusable procedure while the human-facing
	// context lives where the Web UI actually renders it (task_triage
	// dispatch previously crammed everything into Instruction, which left
	// the child task's description empty — nose 2026-08-14 feedback).
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

// Suggestion is task_triage.detail.suggestion — the triage agent's single
// recommendation, derived from primary sources (docs/plans/
// cross-project-issue-triage.md:544 のスキーマ案, 逆輸入3). Verb uses the
// same vocabulary as nose's own response words: go/shape/manual/park/
// drop/wake.
type Suggestion struct {
	Verb   string `json:"verb,omitempty"`
	Action string `json:"action,omitempty"`
	Reason string `json:"reason,omitempty"`
	Basis  string `json:"basis,omitempty"`
}

// DetailSuggestion extracts task_triage.detail.suggestion from a detail
// blob, preferring the top-level key (docs/plans/cross-project-issue-triage.md:544
// のスキーマ案) and falling back to detail.attrs.suggestion. The fallback is
// the one actually exercised today: "suggestion" is not a promoted
// attrs_set key (see applyAttrsSetSideEffect / FoldDetailAttrs,
// internal/api/workflow_card.go), and attrs_set is currently the only
// write path for it in this repo — so in practice it lands under
// detail.attrs, not at the top level. Top-level is still checked first per
// BD-8's design (a future/alternate writer could place it there directly);
// this doc comment just flags that assumption as unverified against actual
// writers, not as dead weight to remove.
//
// Spot-checked against real data 2026-08-18 (`GET /api/triage?status=parked`,
// 4 parked tasks, 3 with a suggestion): consistent with the code-level fact
// above — every suggestion was under detail.attrs.suggestion, top-level was
// null on all 4. This is a one-time, small-N sample, not independent proof
// that no writer ever places it at the top level (that's the attrs_set
// argument above); treat it as "nothing observed to contradict the code-level
// reasoning," not as a standing guarantee that would need re-verifying if
// this comment is trusted later. Both paths are kept regardless — see
// decodeSuggestion's doc comment for why an empty top-level object must not
// win over a real one in attrs.
//
// Each candidate (top-level, then attrs) is decoded independently via its
// own json.Unmarshal call: a malformed or wrong-shaped value in one
// location must not blank out a well-formed value in the other. (An
// earlier version decoded both off a single struct/Unmarshal call, so one
// bad field anywhere in the blob silently zeroed a good suggestion
// elsewhere in the same blob — Opus review finding, 2026-08-18.)
//
// Returns the zero Suggestion and false on any absence/parse failure —
// same best-effort posture as triageSummary (internal/api/tree.go): never
// errors, so a missing/malformed suggestion blob never sinks the row it
// lives on (rule 5, 隠さない, is about the row surviving, not every derived
// field always being present).
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
// the row would render with no suggestion at all (BD-8 post-merge review,
// 2026-08-18 — no writer places a non-empty object at the top level today,
// see DetailSuggestion's doc comment, so this is a guard against a future
// writer, not a fix for an observed bug).
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
// DetailSuggestion above — see its own doc comment), for a caller that needs
// MORE than the fixed Suggestion struct's four fields expose. accept(verb)
// (docs/plans/suggestion-as-state-transition-impl.md §3,
// internal/api/suggestion_accept.go) is the concrete reason this exists:
// park's params.wake_at/wake_task_id are a WRITE-side addition Suggestion
// itself does not carry (Suggestion is the stable read/display-side
// projection this PR does not otherwise touch). Reuses decodeSuggestion
// purely as the "is this candidate non-empty and well-formed" check, so the
// two functions can never disagree about WHICH candidate wins.
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
// ever finishing (the failure mode task_triage's child docs already warn
// about, hit in production 2026-08-21).
//
// A "dispatched" child is refused. That status is the daemon's own
// mechanical fact (論点9) and its task is still running: closing it here
// would let ShouldAutoDone fire while real work is in flight. Withdrawing
// live work is an abort of the child's task, not a triage-detail edit.
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
