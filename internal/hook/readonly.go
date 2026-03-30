package hook

import "github.com/novshi-tech/boid/internal/model"

// IsReadonly returns true if the task's working directory should be mounted read-only.
// This is the case when the behavior itself is readonly (e.g. plan tasks),
// or when the task status is verifying or in_review.
func IsReadonly(behavior *model.TaskBehavior, status model.TaskStatus) bool {
	return behavior.Readonly ||
		status == model.TaskStatusVerifying ||
		status == model.TaskStatusInReview
}
