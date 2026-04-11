package orchestrator

// IsReadonly returns true if the task's working directory should be mounted read-only.
// This is the case when the task itself is readonly (e.g. plan tasks),
// or when the task status is verifying.
func IsReadonly(task *Task) bool {
	return task.Readonly ||
		task.Status == TaskStatusVerifying
}
