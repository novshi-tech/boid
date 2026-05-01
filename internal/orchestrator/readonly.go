package orchestrator

// IsReadonly returns true if the task's working directory should be mounted read-only.
// Driven solely by the task.readonly flag (e.g. plan tasks).
func IsReadonly(task *Task) bool {
	return task.Readonly
}
