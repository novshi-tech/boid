package apiwire

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type NotifyTaskRequest struct {
	Message    string `json:"message"`
	Ask        string `json:"ask,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Done       string `json:"done,omitempty"`
	Fail       string `json:"fail,omitempty"`
}

type AnswerTaskRequest struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

type UpdateTaskRequest struct {
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	ProjectID    string          `json:"project_id,omitempty"`
	RemoteID     *string         `json:"remote_id,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Instructions json.RawMessage `json:"instructions,omitempty"`
	ParentID     *string         `json:"parent_id,omitempty"`
	AutoStart    *bool           `json:"auto_start,omitempty"`
}

type CreateTaskRequest struct {
	ID           string                     `json:"id,omitempty"`
	ProjectID    string                     `json:"project_id"`
	Title        string                     `json:"title"`
	Description  string                     `json:"description,omitempty"`
	Behavior     string                     `json:"behavior,omitempty"`
	BehaviorSpec *orchestrator.BehaviorSpec `json:"behavior_spec,omitempty"`
	RemoteID     string                     `json:"remote_id,omitempty"`
	Payload      json.RawMessage            `json:"payload,omitempty"`
	Instructions json.RawMessage            `json:"instructions,omitempty"`
	AutoStart    bool                       `json:"auto_start,omitempty"`
	Traits       []string                   `json:"traits,omitempty"`
	Ref          string                     `json:"ref,omitempty"`
	ParentID     string                     `json:"parent_id,omitempty"`
	Readonly     *bool                      `json:"readonly,omitempty"`
}

type DuplicateTaskRequest struct {
	AutoStart bool `json:"auto_start"`
}

type RerunTaskRequest struct {
	AutoStart            bool            `json:"auto_start,omitempty"`
	InstructionsOverride json.RawMessage `json:"instructions_override,omitempty"`
}
