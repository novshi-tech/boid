package orchestrator

// ComputeHeadBranch returns the HEAD branch that the given task will occupy
// when its worktree is created.
//
// Root tasks (ParentID == "") occupy the project's base_branch directly.
// Child tasks (ParentID != "") get an isolated "boid/<id8>" branch derived
// from the first 8 characters of their task ID.
//
// Used by BranchLockManager to derive the compound lock key so that tasks on
// the same HEAD branch are serialized while tasks on different branches run
// in parallel.
func ComputeHeadBranch(task *Task) string {
	if task.ParentID == "" {
		return task.BaseBranch
	}
	short := task.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return "boid/" + short
}
