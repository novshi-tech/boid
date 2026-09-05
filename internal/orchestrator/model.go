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

	// Card machine v2 status vocabulary. captured/triaged/ready — earlier
	// pre-execution statuses — are deliberately GONE: no rule in
	// NewCardMachine produces them any more, and a migration converted
	// every remaining pre-cutover row carrying one of them to "parked" and
	// dropped the values from the DB entirely. Re-introducing a reference
	// to them here would silently reopen a vocabulary the schema's CHECK
	// constraint no longer accepts.
	TaskStatusParked  TaskStatus = "parked"
	TaskStatusWorking TaskStatus = "working"
	TaskStatusDropped TaskStatus = "dropped"
)

// KnownTaskStatuses は boid が認識する全 TaskStatus 値を返す。バリデーションを行う全箇所
// (CreateTaskRequest.InitialStatus・brokered task_list の status 検証等) はここを唯一の
// 情報源として参照する — 新しい状態を追加する際にリストがずれないようにするため。
//
// captured/triaged/ready はこのリストから除外済み: DB の CHECK 制約が拒否する
// 値であり、ここに残すとどの行も実際には持ち得ない値をバリデーションが受理
// してしまう。
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
// still be edited, given the task's type and current status:
//   - Card: editable only in "parked", the card's resting/undecided state
//     (a 整形セッション needs to edit these). parked is a card's INITIAL
//     state (not merely a later "set aside" one), so excluding it would
//     make every fresh card permanently uneditable.
//   - Execution: editable only in "pending" (execution not yet started).
//
// working is deliberately not editable: once a card has specced/dispatched
// children or manual work underway, re-editing its title/project out from
// under that is still the wrong default.
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

// TaskType is the discriminator: every Task is either a Card (a judgment
// ledger row — no agent session ever runs against it) or an ExecutionTask
// (a task that runs agent sessions). The two are mutually exclusive and a
// task never changes type over its lifetime.
type TaskType string

const (
	TaskTypeCard      TaskType = "card"
	TaskTypeExecution TaskType = "execution"
)

// Task is the tagged-union encoding of Task = Card | ExecutionTask. Go has
// no native sum type, so this is the "Go flavored" direct sum: a common
// core plus exactly one of Card/Exec populated, selected by Type. Card is
// non-nil iff Type == TaskTypeCard; Exec is non-nil iff Type ==
// TaskTypeExecution — enforced by store.go's scan/insert and by
// CreateTask's construction, mirroring the DB's own CHECK constraint.
//
// Deliberately NOT an interface + two concrete types: that would force a
// signature change (and hand-rolled polymorphic JSON/DB-scan code) across
// every layer that passes *Task around (store/api/web/cmd/client, including
// the portable Mac/Win client, which is only allowed to import boid's Go
// types). A caller that reaches for an execution-only field directly must
// go through task.Exec.X or task.Card.X — a compile error instead of a
// silently wrong zero value.
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
	// IdempotencyKey backs `boid task create --idempotency-key`. Unlike Ref,
	// this is NOT an external-world identity — no link/drop/reopen semantics
	// ride on it (that's task_identities' job — see its own doc comment for
	// the distinction). It exists purely so a caller with no external
	// identity to key off of (the common case: a judgment task minting a
	// child task) can make a create call safe to retry: a second CreateTask
	// with the same (ProjectID, ParentID, IdempotencyKey) returns the FIRST
	// task instead of inserting a duplicate (CreateTask's get-or-create,
	// store.go). Empty string means "no key" (stored as SQL NULL).
	//
	// Scoped by ParentID as well as ProjectID — NOT project-only: two
	// DIFFERENT parent tasks in the same project that happened to reuse the
	// same idempotency_key (e.g. both simply used "step-1") would otherwise
	// silently collide, with the second parent's create call handing back
	// the FIRST parent's child.
	//
	// Caller contract this implies: a judgment task minting a child needs
	// ParentID itself to be STABLE across retries for the dedup to actually
	// catch a duplicate create — e.g. embedding the CARD's id, not the
	// judgment task's own. A trigger re-dispatches a fresh judgment task
	// instance (a new, distinct task id) on every firing, so if a caller
	// passed the judgment task's OWN id as ParentID (the `BOID_TASK_ID`
	// auto-fill default — see cmd/task.go/boid_shim.go), a resend after a
	// crash would carry a DIFFERENT ParentID than the crashed attempt and
	// the dedup would silently miss. Callers using IdempotencyKey for
	// cross-retry convergence MUST pass an explicit ParentID that outlives
	// the creating task itself (e.g. the persistent card's task id), not
	// rely on the BOID_TASK_ID default — see
	// internal/skills/data/boid-task/SKILL.md's "Resume: reconcile before
	// create" section.
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

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
	// at TaskStatusAwaiting, so the list row's child rollup can surface "⚠
	// N" without a second query: a card whose dispatched child asked a
	// question was previously invisible from the parent's own row. The
	// detail page's child ledger has its own equivalent
	// (ChildRow.AwaitingQuestionID); this is the list-side counterpart.
	AwaitingChildCount int `json:"awaiting_child_count,omitempty"`
	// Blocked は表示用フィールド（DBには保存しない）
	Blocked bool `json:"blocked,omitempty"`
}

// ExecAttrs is the execution-only field set: everything that used to live
// flat on Task and drives an actual sandbox dispatch. Non-nil only on a
// Task whose Type == TaskTypeExecution — a Card never carries one, so a
// dead, unexpanded BaseBranch template on a card is now structurally
// impossible instead of merely unused.
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
// t's own Exec/Card struct. Because Exec/Card are POINTERS, a bare `newTask
// := *task` only copies the pointer VALUE — both tasks would keep pointing
// at the SAME ExecAttrs/CardAttrs unless this helper (or equivalent)
// re-points the clone at its own copy. Every call site that mutates a
// cloned task's Exec/Card field must go through this (machine.go's
// Apply/AdvanceFull, coordinator.go's
// DispatchAndAdvance/evaluateAndAdvance, api.attrs_set_done.go's
// done-status noop).
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
	// 書き込み側が未対応)。
	Actor string `json:"actor,omitempty"`
}
