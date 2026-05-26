package orchestrator

// ComputeHeadBranch returns the HEAD branch that the given task will occupy
// when its worktree is created.
//
// Root tasks (ParentID == "") occupy the project's base_branch directly.
// Worktree-less tasks (Worktree == false) also occupy base_branch directly in
// the host project dir; they do not get an isolated boid/<id8> branch. This
// covers Phase 2-2 supervisor case 1 (host HEAD already on base_branch) where
// the supervisor runs in the project root without creating its own worktree.
// Other child tasks (ParentID != "" && Worktree == true) get an isolated
// "boid/<id8>" branch derived from the first 8 characters of their task ID.
//
// Used by BranchLockManager to derive the compound lock key so that tasks on
// the same HEAD branch are serialized while tasks on different branches run
// in parallel, and by the worktree resolver to compute the fork point for
// child tasks (fork from the parent's HEAD branch).
func ComputeHeadBranch(task *Task) string {
	if task.ParentID == "" || !task.Worktree {
		return task.BaseBranch
	}
	short := task.ID
	if len(short) > 8 {
		short = short[:8]
	}
	return "boid/" + short
}
