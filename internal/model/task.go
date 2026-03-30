package model

import (
	"encoding/json"
	"time"
)

type TaskStatus string

const (
	TaskStatusPending            TaskStatus = "pending"
	TaskStatusExecuting          TaskStatus = "executing"
	TaskStatusVerifying          TaskStatus = "verifying"
	TaskStatusInReview           TaskStatus = "in_review"
	TaskStatusCollectingFeedback TaskStatus = "collecting_feedback"
	TaskStatusDone               TaskStatus = "done"
	TaskStatusAborted            TaskStatus = "aborted"
)

type Task struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	RemoteID     string          `json:"remote_id,omitempty"`
	DataSourceID string          `json:"datasource_id,omitempty"`
	Title        string          `json:"title"`
	Description  string          `json:"description,omitempty"`
	Status       TaskStatus      `json:"status"`
	Behavior     string          `json:"behavior"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type TaskBehavior struct {
	Name         string   `yaml:"name" json:"name"`
	Transition   string   `yaml:"transition" json:"transition"`
	Traits       []string `yaml:"traits" json:"traits"`
	Worktree     bool     `yaml:"worktree" json:"worktree,omitempty"`
	BranchPrefix string   `yaml:"branch_prefix" json:"branch_prefix,omitempty"`
	BaseBranch   string   `yaml:"base_branch" json:"base_branch,omitempty"`
}
