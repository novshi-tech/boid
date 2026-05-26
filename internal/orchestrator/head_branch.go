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
// in parallel. For the worktree resolver's fork-point computation (which has
// different semantics for worktree-less parents), use ComputeForkPoint.
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

// ComputeForkPoint returns the branch a child task should fork its worktree
// from, given the parent task. It differs from ComputeHeadBranch in that
// worktree-less parents (Worktree == false) return their BaseBranch rather
// than the synthetic "boid/<id8>" branch — because a worktree=false parent
// runs in the host project dir on its base_branch and never creates a
// boid/<id8> branch. Falling back to base_branch is required for the Phase
// 2-2 supervisor case 1 path (host HEAD already on base_branch); otherwise
// child worktree creation fails with "fork point boid/<id8> not found
// locally (parent task worktree missing?)".
func ComputeForkPoint(parent *Task) string {
	if parent == nil {
		return ""
	}
	if !parent.Worktree {
		return parent.BaseBranch
	}
	return ComputeHeadBranch(parent)
}
