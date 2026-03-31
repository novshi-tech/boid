package dispatcher

import "github.com/novshi-tech/boid/internal/model"

// Type aliases re-export model job types so consumers can reference dispatcher.Job, etc.
type Job = model.Job
type JobStatus = model.JobStatus

const (
	JobStatusRunning   = model.JobStatusRunning
	JobStatusCompleted = model.JobStatusCompleted
	JobStatusFailed    = model.JobStatusFailed
)

// JobCompletionResult is the result delivered via WaitForJobCtx/CompleteJob.
type JobCompletionResult struct {
	Output   string // stdout capture (payload_patch JSON)
	ExitCode int
}
