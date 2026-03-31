package orchestrator

import "github.com/novshi-tech/boid/internal/model"

// Type aliases: orchestrator is the canonical location for task/action types.
// Underlying definitions live in model/ during the migration period.

type Task = model.Task
type TaskStatus = model.TaskStatus
type Action = model.Action

const (
	TaskStatusPending            = model.TaskStatusPending
	TaskStatusExecuting          = model.TaskStatusExecuting
	TaskStatusVerifying          = model.TaskStatusVerifying
	TaskStatusInReview           = model.TaskStatusInReview
	TaskStatusCollectingFeedback = model.TaskStatusCollectingFeedback
	TaskStatusDone               = model.TaskStatusDone
	TaskStatusAborted            = model.TaskStatusAborted
)
