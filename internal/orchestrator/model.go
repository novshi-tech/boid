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
// started) plus captured/triaged/ready (triage task not yet dispatched — a
// 整形セッション, UC-3, needs to edit these). parked is deliberately excluded
// — a parked task is set aside and not the target of active editing.
func IsPreDispatchEditableStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusPending, TaskStatusCaptured, TaskStatusTriaged, TaskStatusReady:
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
}
