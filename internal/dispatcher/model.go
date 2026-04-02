package dispatcher

import "time"

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

type Job struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	ProjectID   string    `json:"project_id"`
	HandlerID   string    `json:"handler_id"`
	Role        string    `json:"role"`
	RuntimeID   string    `json:"runtime_id,omitempty"`
	Interactive bool      `json:"interactive"`
	TTY         bool      `json:"tty"`
	Status      JobStatus `json:"status"`
	ExitCode    int       `json:"exit_code,omitempty"`
	Output      string    `json:"output,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// JobCompletionResult is the result delivered via WaitForJobCtx/CompleteJob.
type JobCompletionResult struct {
	Output   string
	ExitCode int
}
