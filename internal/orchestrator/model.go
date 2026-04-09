package orchestrator

import (
	"encoding/json"
	"time"
)

type TaskStatus string

const (
	TaskStatusPending            TaskStatus = "pending"
	TaskStatusExecuting          TaskStatus = "executing"
	TaskStatusReworking          TaskStatus = "reworking"
	TaskStatusVerifying          TaskStatus = "verifying"
	TaskStatusInReview           TaskStatus = "in_review"
	TaskStatusCollectingFeedback TaskStatus = "collecting_feedback"
	TaskStatusDone               TaskStatus = "done"
	TaskStatusAborted            TaskStatus = "aborted"
)

type StartGate struct {
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

type Task struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	RemoteID     string          `json:"remote_id,omitempty"`
	DataSourceID string          `json:"datasource_id,omitempty"`
	Title        string          `json:"title"`
	Description  string          `json:"description,omitempty"`
	Status       TaskStatus      `json:"status"`
	Behavior     string          `json:"behavior"`
	Transition   string          `json:"transition"`
	Traits       []string        `json:"traits,omitempty"`
	Readonly     bool            `json:"readonly,omitempty"`
	Worktree     bool            `json:"worktree,omitempty"`
	BranchPrefix string          `json:"branch_prefix,omitempty"`
	BaseBranch   string          `json:"base_branch,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	AutoStart    bool            `json:"auto_start,omitempty"`
	DependsOn    []string        `json:"depends_on,omitempty"`
	StartGate    *StartGate      `json:"start_gate,omitempty"`
	Ref          string          `json:"ref,omitempty"`
	ParentID     string          `json:"parent_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type Action struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"task_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}
