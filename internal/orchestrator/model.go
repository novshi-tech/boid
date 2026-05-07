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
)

type Task struct {
	ID               string          `json:"id"`
	ProjectID        string          `json:"project_id"`
	RemoteID         string          `json:"remote_id,omitempty"`
	DataSourceID     string          `json:"datasource_id,omitempty"`
	Title            string          `json:"title"`
	Description      string          `json:"description,omitempty"`
	Status           TaskStatus      `json:"status"`
	Behavior         string          `json:"behavior"`
	Traits           []string        `json:"traits,omitempty"`
	Readonly         bool            `json:"readonly,omitempty"`
	Worktree         bool            `json:"worktree,omitempty"`
	BranchPrefix     string          `json:"branch_prefix,omitempty"`
	BaseBranch       string          `json:"base_branch,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	Instructions     Instructions    `json:"instructions,omitempty"`
	AutoStart        bool            `json:"auto_start,omitempty"`
	DependsOn        []string        `json:"depends_on,omitempty"`
	DependsOnPayload string          `json:"depends_on_payload,omitempty"`
	Ref              string          `json:"ref,omitempty"`
	ParentID         string          `json:"parent_id,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	// 以下はDBに保存しない派生フィールド（list/get クエリ時にサブクエリで集計）
	TotalChildCount   int `json:"total_child_count,omitempty"`
	DoneChildCount    int `json:"done_child_count,omitempty"`
	AbortedChildCount int `json:"aborted_child_count,omitempty"`
	OpenChildCount    int `json:"open_child_count,omitempty"`
	// Blocked は pending 状態でかつ依存条件が未充足のとき true（DBには保存しない）
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
