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

	// 以下は cross-project-issue-triage Phase 1 (docs/plans/cross-project-issue-triage.md) の
	// pre-execution 状態。captured/triaged/parked/ready は「まだ実行タスクではない」段階を表す。
	// dropped は done/aborted と並ぶ終端状態だが、nose の明示判断でしか入らない点で区別する
	// (逆輸入2: 「終わった件と捨てた件を区別できないと成功の定義が測れない」)。
	TaskStatusCaptured TaskStatus = "captured"
	TaskStatusTriaged  TaskStatus = "triaged"
	TaskStatusParked   TaskStatus = "parked"
	TaskStatusReady    TaskStatus = "ready"
	TaskStatusDropped  TaskStatus = "dropped"

	// TaskStatusWorking is Phase 1 PR-2's addition (逆輸入2: dispatched は
	// working に再定義された). Deliberately NOT classified as pre-execution
	// (see IsPreExecutionStatus below): unlike captured/triaged/parked/ready,
	// a working triage task has already been through Go — it is closer in
	// kind to executing/awaiting (something is actively happening, whether
	// that's a dispatched child task or nose working the item by hand) than
	// to the "not yet actionable" pre-execution states. Concretely this means
	// working:
	//   - is NOT excluded from the default "open" task-list filter the way
	//     triaged/parked/ready are (store.go's notOpenSelfStatusSQLList) —
	//     an in-progress triage task belongs in the open view, same as an
	//     executing task. (captured is ALSO not excluded as of
	//     docs/plans/ingestion-identity.md PR-2, B-2 — see
	//     notOpenSelfStatusSQLList's own doc comment — so "pre-execution
	//     statuses are excluded" is no longer true of the whole group;
	//     triaged/parked/ready remain the excluded ones.)
	//   - historically did NOT appear in the "queue" filter
	//     (preExecutionStatusSQLList) — queue was for things nose still
	//     needed to respond to (決定9), and a working task has already been
	//     responded to (Go'd). That broad "queue" superset filter was removed
	//     in PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1,
	//     unused by the Web UI); its replacement, "queue_next", is now
	//     suggestion-driven (store.go) — a working card DOES appear there if
	//     it carries a suggestion (e.g. khi suggesting "done" or "park" on
	//     in-progress work), unlike the old status-based exclusion.
	//   - is NOT GC'd via the pre-execution "keep forever" carve-out; it
	//     follows the same non-terminal "never auto-GC'd" rule executing/
	//     awaiting already get, which is the correct default until PR-3's
	//     queue evaluation defines what "stuck in working" watchdog behavior
	//     (if any) should look like.
	// PR-2 gives working exactly one entry (ready→working, machine-driven,
	// see machine.go's "dispatch" rule) and deliberately NO exit yet — see
	// that rule's own doc comment for why this is left as a known PR-3 gap
	// rather than guessed at here.
	TaskStatusWorking TaskStatus = "working"
)

// KnownTaskStatuses は boid が認識する全 TaskStatus 値を返す。バリデーションを行う全箇所
// (CreateTaskRequest.InitialStatus・brokered task_list の status 検証等) はここを唯一の
// 情報源として参照する — 新しい状態を追加する際にリストがずれないようにするため。
func KnownTaskStatuses() []TaskStatus {
	return []TaskStatus{
		TaskStatusPending,
		TaskStatusExecuting,
		TaskStatusAwaiting,
		TaskStatusDone,
		TaskStatusAborted,
		TaskStatusCaptured,
		TaskStatusTriaged,
		TaskStatusParked,
		TaskStatusReady,
		TaskStatusDropped,
		TaskStatusWorking,
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

// IsPreExecutionStatus は task が「まだ実行タスクとして動いていない」triage 段階かを返す
// (captured/triaged/parked/ready)。既存の pending/executing/awaiting とは区別し、デフォルトの
// open ビューを汚さないために使う。
//
// captured/triaged/ready are legacy-only under card machine v2 (docs/plans/
// suggestion-as-state-transition-impl.md): no rule anywhere in
// NewCardMachine ever produces them anymore (a fresh card starts, and stays,
// in parked until a human/khi-accepted transition moves it). They remain in
// this predicate purely so a pre-cutover DB row still carrying one of them is
// filtered/edited the same way it always was, not silently reclassified —
// see IsPreDispatchEditableStatus's own doc comment for the identical
// reasoning applied to editability specifically.
func IsPreExecutionStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusCaptured, TaskStatusTriaged, TaskStatusParked, TaskStatusReady:
		return true
	default:
		return false
	}
}

// IsPreDispatchEditableStatus reports whether title/project/instructions can
// still be edited in the given status: pending (unchanged, execution not yet
// started) plus parked (a card's main resting/undecided state — a 整形
// セッション, UC-3, needs to edit these) and the legacy captured/triaged/
// ready statuses (kept editable too, purely so a pre-cutover DB row is not
// newly locked out of editing by this same change; card machine v2 has no
// rule reaching any of the three).
//
// parked WAS deliberately excluded pre-v2 ("a parked task is set aside and
// not the target of active editing") — that premise broke when card machine
// v2 (docs/plans/suggestion-as-state-transition-impl.md §3.5) folded
// captured/triaged into parked: parked is now a card's INITIAL state (not
// just a later "set aside" one), so excluding it made every fresh card
// permanently uneditable (title/project/instructions), with only
// description slipping through (UpdateTask's status guard only covers
// those three fields) — a real regression the v1→v2 status collapse
// introduced silently (PR #987 review, HIGH 7). working is unchanged:
// once a card has specced/dispatched children or manual work underway,
// re-editing its title/project out from under that is still the wrong
// default.
func IsPreDispatchEditableStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusPending, TaskStatusParked, TaskStatusCaptured, TaskStatusTriaged, TaskStatusReady:
		return true
	default:
		return false
	}
}

type Task struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	RemoteID     string          `json:"remote_id,omitempty"`
	Title        string          `json:"title"`
	Description  string          `json:"description,omitempty"`
	Status       TaskStatus      `json:"status"`
	Behavior     string          `json:"behavior"`
	Traits       []string        `json:"traits,omitempty"`
	Readonly     bool            `json:"readonly,omitempty"`
	BranchPrefix string          `json:"branch_prefix,omitempty"`
	BaseBranch   string          `json:"base_branch,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Instructions Instructions    `json:"instructions,omitempty"`
	AutoStart    bool            `json:"auto_start,omitempty"`
	Ref          string          `json:"ref,omitempty"`
	ParentID     string          `json:"parent_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	// 以下はDBに保存しない派生フィールド（list/get クエリ時にサブクエリで集計）
	TotalChildCount   int `json:"total_child_count,omitempty"`
	DoneChildCount    int `json:"done_child_count,omitempty"`
	AbortedChildCount int `json:"aborted_child_count,omitempty"`
	OpenChildCount    int `json:"open_child_count,omitempty"`
	// Blocked は表示用フィールド（DBには保存しない）
	Blocked bool `json:"blocked,omitempty"`
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
