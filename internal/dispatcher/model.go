package dispatcher

import "time"

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

type Job struct {
	ID                 string    `json:"id"`
	TaskID             string    `json:"task_id"`
	ProjectID          string    `json:"project_id"`
	HandlerID          string    `json:"handler_id"`
	DisplayName        string    `json:"display_name,omitempty"` // persisted via the jobs.display_name column (migration 0027)
	TerminalTitle      string    `json:"terminal_title,omitempty"`
	DisplayNameDefault bool      `json:"display_name_default,omitempty"`
	Role               string    `json:"role"`
	RuntimeID          string    `json:"runtime_id,omitempty"`
	Interactive        bool      `json:"interactive"`
	TTY                bool      `json:"tty"`
	Status             JobStatus `json:"status"`
	ExitCode           int       `json:"exit_code,omitempty"`
	Output             string    `json:"output,omitempty"`
	ExecutionState     string    `json:"execution_state,omitempty"`
	// CardID / CardRequestID mirror orchestrator.JobSpec.CardID/CardRequestID
	// at the instant this job was dispatched — persisted (unlike the
	// broker's in-memory token registry) so a daemon restart's
	// card_requests recovery scan can reverse-lookup a launching request's
	// session continuation from this table alone (see
	// internal/orchestrator/card_request_release.go's
	// RecoverLaunchingCardRequests). Empty for every job that isn't a card
	// command's launcher or continuation.
	CardID        string    `json:"card_id,omitempty"`
	CardRequestID string    `json:"card_request_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// JobCompletionResult is the result delivered via WaitForJobCtx/CompleteJob.
type JobCompletionResult struct {
	Output   string
	ExitCode int
}

// EffectiveDisplayName prefers an explicit name, then the terminal title,
// then the name generated when the job was launched.
func (j *Job) EffectiveDisplayName() string {
	if j.DisplayName != "" && !j.DisplayNameDefault {
		return j.DisplayName
	}
	if j.TerminalTitle != "" {
		return j.TerminalTitle
	}
	return j.DisplayName
}
