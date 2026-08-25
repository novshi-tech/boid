package orchestrator

// IsReadonly returns true if the task's working directory should be mounted
// read-only. Driven solely by the task's Exec.Readonly flag (e.g. plan
// tasks) — Readonly is execution-only (design doc §3.2); a card (Exec ==
// nil) never mounts a sandbox at all, so false is the only sensible answer.
func IsReadonly(task *Task) bool {
	if task == nil || task.Exec == nil {
		return false
	}
	return task.Exec.Readonly
}
