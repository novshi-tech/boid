package api

import "time"

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

type Job struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"task_id"`
	ProjectID      string    `json:"project_id"`
	HandlerID      string    `json:"handler_id"`
	DisplayName    string    `json:"display_name,omitempty"`
	Role           string    `json:"role"`
	RuntimeID      string    `json:"runtime_id,omitempty"`
	WorkspacePath  string    `json:"workspace_path,omitempty"`
	Interactive    bool      `json:"interactive"`
	TTY            bool      `json:"tty"`
	Status         JobStatus `json:"status"`
	ExitCode       int       `json:"exit_code,omitempty"`
	Output         string    `json:"output,omitempty"`
	ExecutionState        string     `json:"execution_state,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	TranscriptSize        int64      `json:"transcript_size,omitempty"`
	TranscriptMtime       *time.Time `json:"transcript_mtime,omitempty"`
	TranscriptIdleSeconds int64      `json:"transcript_idle_seconds,omitempty"`
}

type JobCompletion struct {
	Output   string
	ExitCode int
}

// JobWithContext extends Job with task and project metadata for the TUI global view.
type JobWithContext struct {
	Job
	TaskTitle   string `json:"task_title"`
	ProjectName string `json:"project_name"`
}

// JobListFilter specifies optional filters for global job listing.
type JobListFilter struct {
	Status       string
	Interactive  *bool // nil = no filter
	TasklessOnly bool  // true = only jobs where task_id IS NULL
}
