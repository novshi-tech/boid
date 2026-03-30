package model

import "time"

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

type Job struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	ProjectID string    `json:"project_id"`
	HandlerID string    `json:"handler_id"` // hook ID or gate ID
	Role      string    `json:"role"`       // "hook" or "gate"
	Status    JobStatus `json:"status"`
	ExitCode  int       `json:"exit_code,omitempty"`
	Output    string    `json:"output,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
