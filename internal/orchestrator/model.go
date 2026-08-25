package orchestrator

import (
	"encoding/json"
	"time"
)

type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusExecuting TaskStatus = "executing"
	TaskStatusAwaiting  TaskStatus = "awaiting"
	TaskStatusDone      TaskStatus = "done"
	TaskStatusAborted   TaskStatus = "aborted"

	// Card machine v2 status vocabulary (docs/plans/
	// suggestion-as-state-transition-impl.md §3.5, carried forward by
	// docs/plans/card-model-cleanup.md §3.3). captured/triaged/ready — the
	// cross-project-issue-triage Phase 1 pre-execution statuses — are
	// deliberately GONE as of the card-model-cleanup PR-2 migration (0045):
	// no rule in NewCardMachine has produced them since card machine v2
	// shipped, and migration 0045 洗い替えs every remaining pre-cutover row
	// carrying one of them to "parked" and drops the values from the DB
	// entirely. Re-introducing a reference to them here would silently
	// reopen a vocabulary the schema's CHECK constraint (migration 0045) no
	// longer accepts.
	TaskStatusParked  TaskStatus = "parked"
	TaskStatusWorking TaskStatus = "working"
	TaskStatusDropped TaskStatus = "dropped"
)

// KnownTaskStatuses は boid が認識する全 TaskStatus 値を返す。バリデーションを行う全箇所
// (CreateTaskRequest.InitialStatus・brokered task_list の status 検証等) はここを唯一の
// 情報源として参照する — 新しい状態を追加する際にリストがずれないようにするため。
//
// card-model-cleanup PR-2 (migration 0045) removed captured/triaged/ready
// from this list: they are no longer valid tasks.status values in the DB
// (the new CHECK constraint rejects them), so keeping them here would let
// validation accept a value no row can ever actually hold.
func KnownTaskStatuses() []TaskStatus {
	return []TaskStatus{
		TaskStatusPending,
		TaskStatusExecuting,
		TaskStatusAwaiting,
		TaskStatusDone,
		TaskStatusAborted,
		TaskStatusParked,
		TaskStatusWorking,
		TaskStatusDropped,
	}
}

// ParseTaskStatus は文字列を既知の TaskStatus に変換する。大文字小文字は区別しない曖昧さを
// 持ち込まない（完全一致のみ許可）。
func ParseTaskStatus(s string) (TaskStatus, bool) {
	for _, known := range KnownTaskStatuses() {
		if string(known) == s {
			return known, true
		}
	}
	return "", false
}

// IsTerminalStatus は task がこれ以上遷移しない終端状態かを返す (done/aborted/dropped)。
// GC 対象判定・open/closed フィルタ・boid task watch の終了判定はすべてここを経由する。
func IsTerminalStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusDone, TaskStatusAborted, TaskStatusDropped:
		return true
	default:
		return false
	}
}

// IsPreDispatchEditableStatus reports whether title/project/instructions can
// still be edited, given the task's type and current status. card-model-
// cleanup PR-2 (docs/plans/card-model-cleanup.md §3.3) retired the old
// status-only predicate (which leaned on now-deleted legacy statuses to
// answer "is this a pre-dispatch card") in favor of a direct type switch:
//   - Card: editable only in "parked", the card's resting/undecided state
//     (a 整形セッション, UC-3, needs to edit these).
//   - Execution: editable only in "pending" (execution not yet started).
//
// parked WAS deliberately excluded pre-card-machine-v2 ("a parked task is
// set aside and not the target of active editing") — that premise broke
// when card machine v2 folded captured/triaged into parked: parked is now a
// card's INITIAL state (not just a later "set aside" one), so excluding it
// made every fresh card permanently uneditable (title/project/instructions),
// with only description slipping through (PR #987 review, HIGH 7). working
// is unchanged: once a card has specced/dispatched children or manual work
// underway, re-editing its title/project out from under that is still the
// wrong default.
func IsPreDispatchEditableStatus(taskType TaskType, status TaskStatus) bool {
	switch taskType {
	case TaskTypeCard:
		return status == TaskStatusParked
	case TaskTypeExecution:
		return status == TaskStatusPending
	default:
		return false
	}
}

// TaskType is the discriminator card-model-cleanup PR-2 (docs/plans/
// card-model-cleanup.md §3) introduces: every Task is either a Card (a
// judgment ledger row — no agent session ever runs against it) or an
// ExecutionTask (a task that runs agent sessions). The two are mutually
// exclusive and a task never changes type over its lifetime (subtype
// modeling's two conditions, §2 of the design doc).
type TaskType string

const (
	TaskTypeCard      TaskType = "card"
	TaskTypeExecution TaskType = "execution"
)

// Task is the tagged-union encoding of Task = Card | ExecutionTask (design
// doc §3.5). Go has no native sum type, so this is the "Go flavored" direct
// sum: a common core plus exactly one of Card/Exec populated, selected by
// Type. Card is non-nil iff Type == TaskTypeCard; Exec is non-nil iff
// Type == TaskTypeExecution — enforced by store.go's scan/insert and by
// CreateTask's construction, mirroring the DB's own CHECK constraint
// (migration 0045).
//
// Deliberately NOT an interface + two concrete types: that would force a
// signature change (and hand-rolled polymorphic JSON/DB-scan code) across
// every layer that passes *Task around (store/api/web/cmd/client, including
// the portable Mac/Win client, which is only allowed to import boid's Go
// types — see design doc §3.5). A caller that reaches for an execution-only
// field directly (task.Behavior, as it read before this PR) now gets a
// compile error instead of a silently wrong zero value — every such
// reference had to be individually reviewed and pointed at task.Exec.X or
// task.Card.X to make this PR compile, which is this refactor's built-in
// verification mechanism.
type Task struct {
	ID          string     `json:"id"`
	Type        TaskType   `json:"type"`
	ProjectID   string     `json:"project_id"`
	RemoteID    string     `json:"remote_id,omitempty"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Status      TaskStatus `json:"status"`
	Ref         string     `json:"ref,omitempty"`
	ParentID    string     `json:"parent_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// Card holds the card-only fields (kind/urgency/wake_at/wake_task_id/
	// suggestion_verb/detail). Non-nil iff Type == TaskTypeCard.
	Card *CardAttrs `json:"card,omitempty"`
	// Exec holds the execution-only fields (behavior/traits/readonly/
	// branch_prefix/base_branch/payload/instructions/auto_start). Non-nil
	// iff Type == TaskTypeExecution.
	Exec *ExecAttrs `json:"exec,omitempty"`

	// 以下はDBに保存しない派生フィールド（list/get クエリ時にサブクエリで集計）
	TotalChildCount   int `json:"total_child_count,omitempty"`
	DoneChildCount    int `json:"done_child_count,omitempty"`
	AbortedChildCount int `json:"aborted_child_count,omitempty"`
	OpenChildCount    int `json:"open_child_count,omitempty"`
	// AwaitingChildCount is the number of direct children currently sitting
	// at TaskStatusAwaiting — added by docs/plans/webui-detail-list-redesign.md
	// PR-4 (§3.5) so the list row's child rollup can surface "⚠ N" without a
	// second query: a card whose dispatched child asked a question was
	// previously invisible from the parent's own row (§2.4's "子が4回 ask を
	// 上げたが親からは dispatched にしか見えなかった" gap) — the detail page's
	// child ledger already closed this for PR-2 (ChildRow.AwaitingQuestionID);
	// this is the list-side counterpart.
	AwaitingChildCount int `json:"awaiting_child_count,omitempty"`
	// Blocked は表示用フィールド（DBには保存しない）
	Blocked bool `json:"blocked,omitempty"`
}

// ExecAttrs is the execution-only field set (design doc §3.2's field
// attribution table): everything that used to live flat on Task and drives
// an actual sandbox dispatch. Non-nil only on a Task whose Type ==
// TaskTypeExecution — a Card never carries one, so the "嘘の behavior 一式"
// landmine (a card's dead, unexpanded BaseBranch template — see
// task_resolve_or_capture.go's history) is now structurally impossible
// instead of merely unused.
type ExecAttrs struct {
	// Behavior is allowed to be the empty string (a no-yaml project's
	// resolved behavior name) — that is why the DB column backing this
	// field is nullable-but-"IS NOT NULL for execution rows" rather than
	// NOT NULL DEFAULT '': NULL means "not an execution task", "" means "an
	// execution task with no named behavior". See migration 0045's CHECK
	// constraint comment.
	Behavior     string          `json:"behavior"`
	Traits       []string        `json:"traits,omitempty"`
	Readonly     bool            `json:"readonly,omitempty"`
	BranchPrefix string          `json:"branch_prefix,omitempty"`
	BaseBranch   string          `json:"base_branch,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Instructions Instructions    `json:"instructions,omitempty"`
	AutoStart    bool            `json:"auto_start,omitempty"`
}

// CloneTaskShallow returns a shallow copy of t whose Exec/Card pointers (if
// set) are ALSO copied, so a caller that mutates a field on the clone's
// Exec/Card (e.g. `clone.Exec.Payload = merged`) never aliases back into
// t's own Exec/Card struct. Before the tagged-struct split, `newTask :=
// *task` followed by `newTask.Payload = x` was always safe (Payload was a
// value-typed field copied by the struct copy); after the split, Exec/Card
// are POINTERS, so `newTask := *task` only copies the pointer VALUE — both
// tasks would keep pointing at the SAME ExecAttrs/CardAttrs unless this
// helper (or equivalent) re-points the clone at its own copy. Every call
// site that used to do a bare `x := *task` and then mutate a
// now-relocated field must go through this instead (machine.go's Apply/
// AdvanceFull, coordinator.go's DispatchAndAdvance/evaluateAndAdvance,
// api.attrs_set_done.go's done-status noop) — this is exactly the class of
// "縫い目" bug a mechanical find/replace of task.X -> task.Exec.X would
// have missed.
func CloneTaskShallow(t *Task) *Task {
	if t == nil {
		return nil
	}
	clone := *t
	if t.Exec != nil {
		e := *t.Exec
		clone.Exec = &e
	}
	if t.Card != nil {
		c := *t.Card
		clone.Card = &c
	}
	return &clone
}

type Action struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	FromStatus TaskStatus      `json:"from_status,omitempty"`
	ToStatus   TaskStatus      `json:"to_status,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	// Actor は誰/何がこの action を押したか (ActorHuman / ActorDaemon /
	// ActorTask(taskID) のいずれか)。旧レコードは空文字 (移行前データ、または
	// 書き込み側が未対応)。docs/plans/cross-project-issue-triage.md 論点11。
	Actor string `json:"actor,omitempty"`
}
